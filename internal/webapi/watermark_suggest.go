package webapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// --- watermark auto-suggest (FEAT-WM-AUTO) ---
//
// One catalog round-trip pulls every column of every USER table (relkind
// r/p — views and system schemas excluded) with an index flag and the
// DEFAULT-now() signal, then a pure scorer ranks candidate unified
// watermark columns. PG-only (incremental v1 boundary).

// wmCatalogColumn is one row of the catalog query.
type wmCatalogColumn struct {
	Table      string
	Column     string
	DataType   string
	Indexed    bool
	DefaultNow bool
}

// wmSuggestCandidate is the JSON shape of one ranked candidate.
type wmSuggestCandidate struct {
	Column          string         `json:"column"`
	Score           float64        `json:"score"`
	Coverage        float64        `json:"coverage"`
	IndexedRatio    float64        `json:"indexed_ratio"`
	DefaultNowRatio float64        `json:"default_now_ratio"`
	TypeHistogram   map[string]int `json:"type_histogram"`
	MatchedTables   []string       `json:"matched_tables"`
	UnmatchedTables []string       `json:"unmatched_tables"`
	Reasons         []string       `json:"reasons"`
	Warnings        []string       `json:"warnings"`
}

// wmSystemSchemas are never scanned for watermark candidates.
var wmSystemSchemas = map[string]bool{
	"pg_catalog": true, "information_schema": true, "pg_toast": true,
}

// wmDefaultNowRe detects automatically-maintained timestamp defaults.
// The (^|[^']) guard rejects string LITERALS like 'now()'::text (a quoted
// default is a constant, not auto-maintenance); RE2 has no lookbehind, so
// the preceding-character class stands in.
var wmDefaultNowRe = regexp.MustCompile(`(?i)(^|[^'])(now\(\)|current_timestamp|localtimestamp|transaction_timestamp\(\))`)

// queryWMCatalog fetches all columns of all user tables in one query.
// relispartition=false excludes partition CHILDREN (relkind 'r' rows that
// are partitions of a 'p' parent) so the catalog counts logical tables,
// not one entry per partition; indisvalid skips failed/leftover invalid
// indexes from the index flag.
// wmCatalogSQL is a named const so tests can pin its structural guards
// (partition exclusion, valid-index-only) — the SQL's real semantics are
// covered by isolation testing against a live PG.
const wmCatalogSQL = `
		SELECT c.table_name, c.column_name, c.data_type,
		       COALESCE(c.column_default, ''),
		       EXISTS (SELECT 1 FROM pg_index i
		                JOIN LATERAL unnest(i.indkey) WITH ORDINALITY k(attnum, ord) ON true
		                JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
		               WHERE i.indrelid = t.oid AND i.indisvalid AND a.attname = c.column_name)
		FROM information_schema.columns c
		JOIN pg_class t ON t.relname = c.table_name
		JOIN pg_namespace n ON n.oid = t.relnamespace AND n.nspname = c.table_schema
		WHERE c.table_schema = $1
		  AND t.relkind IN ('r', 'p')
		  AND NOT t.relispartition
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
		ORDER BY c.table_name, c.ordinal_position`

func queryWMCatalog(ctx context.Context, db *sql.DB, schema string) ([]wmCatalogColumn, error) {
	rows, err := db.QueryContext(ctx, wmCatalogSQL, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []wmCatalogColumn
	for rows.Next() {
		var c wmCatalogColumn
		var def string
		var idxed bool
		if err := rows.Scan(&c.Table, &c.Column, &c.DataType, &def, &idxed); err != nil {
			return nil, err
		}
		c.Indexed = idxed
		c.DefaultNow = wmDefaultNowRe.MatchString(def)
		out = append(out, c)
	}
	return out, rows.Err()
}

// wmNameNorm folds camelCase / snake_case / kebab-case into one comparable
// form: lowercase with separators stripped ("updatedAt" == "updated_at").
func wmNameNorm(s string) string {
	s = strings.ToLower(s)
	return strings.NewReplacer("_", "", "-", "", " ", "").Replace(s)
}

// Update-series strong names: columns that actually advance on UPDATE.
var wmStrongNames = map[string]bool{
	"updatedat": true, "updatetime": true, "modifiedat": true,
	"modifytime": true, "lastmodified": true, "lastmodifiedtime": true,
	"gmtmodified": true, "updatedate": true, "modificationtime": true,
	"lastupdatetime": true, "lastupdate": true, "updatetimestamp": true,
}

// Created-series: monotonic but only on INSERT — demoted with a warning.
var wmCreatedNames = map[string]bool{
	"createdat": true, "createtime": true, "gmtcreate": true,
	"insertedat": true, "inserttime": true, "createddate": true,
	"creationtime": true, "created": true,
}

// wmNameClass returns (weight, label) for a column name. The id-family
// check runs on the RAW lowercased name (requiring a separator before
// "id") — the normalized form would also catch valid/grid/guid/userid,
// which are not auto-increment semantics.
func wmNameClass(name string) (float64, string) {
	n := wmNameNorm(name)
	raw := strings.ToLower(name)
	switch {
	case wmStrongNames[n]:
		return 1.0, "更新系命名"
	case wmCreatedNames[n]:
		return 0.4, "创建系命名"
	case raw == "id" || strings.HasSuffix(raw, "_id") || strings.HasSuffix(raw, "-id"):
		return 0.1, "自增系命名"
	default:
		return 0.0, ""
	}
}

// wmTypeWeight grades comparable watermark types. Membership is anchored
// to the single incWatermarkTypes source (same package) so the two sets
// can never drift; this switch only assigns the relative weight.
func wmTypeWeight(dataType string) (float64, string) {
	if !incWatermarkTypes[dataType] {
		return 0, ""
	}
	switch dataType {
	case "timestamp with time zone":
		return 1.0, "timestamptz"
	case "timestamp without time zone":
		return 0.9, "timestamp"
	case "date":
		return 0.7, "date"
	case "bigint":
		return 0.5, "bigint"
	case "integer":
		return 0.4, "integer"
	default:
		return 0, ""
	}
}

// scoreWatermarkCandidates ranks unified watermark column candidates over
// the catalog. Pure function — fully unit-testable without a database.
// A table "matches" a candidate when it has that column with a comparable
// type; coverage = matched / total tables. Score (0-100):
//
//	0.35*coverage + 0.25*nameClass + 0.15*avgTypeWeight
//	  + 0.15*indexedRatio + 0.10*defaultNowRatio
//
// Non-dictionary names still participate (a fully indexed timestamptz
// column covering every table can win without a magic name) but start
// from a lower base.
func scoreWatermarkCandidates(cols []wmCatalogColumn) (total int, cands []wmSuggestCandidate) {
	tables := map[string]bool{}
	for _, c := range cols {
		tables[c.Table] = true
	}
	total = len(tables)
	if total == 0 {
		return 0, nil
	}

	type acc struct {
		matched       int
		indexed       int
		defaultNow    int
		typeSum       float64
		hist          map[string]int
		matchedTables []string
		nameWeight    float64
		nameLabel     string
		bestTypeW     float64
		anyDate       bool
		anyComparable bool
	}
	byCol := map[string]*acc{}
	for _, c := range cols {
		tw, label := wmTypeWeight(c.DataType)
		a := byCol[c.Column]
		if a == nil {
			a = &acc{hist: map[string]int{}}
			byCol[c.Column] = a
			a.nameWeight, a.nameLabel = wmNameClass(c.Column)
		}
		if tw > 0 {
			a.anyComparable = true
			a.matched++
			a.typeSum += tw
			if tw > a.bestTypeW {
				a.bestTypeW = tw
			}
			a.hist[label]++
			if c.Indexed {
				a.indexed++
			}
			if c.DefaultNow {
				a.defaultNow++
			}
			if label == "date" {
				a.anyDate = true
			}
			a.matchedTables = append(a.matchedTables, c.Table)
		}
	}

	for name, a := range byCol {
		if !a.anyComparable {
			continue
		}
		cov := float64(a.matched) / float64(total)
		avgType := a.typeSum / float64(a.matched)
		idxRatio := float64(a.indexed) / float64(a.matched)
		nowRatio := float64(a.defaultNow) / float64(a.matched)
		score := 100 * (0.35*cov + 0.25*a.nameWeight + 0.15*avgType + 0.15*idxRatio + 0.10*nowRatio)

		var reasons, warnings []string
		switch a.nameLabel {
		case "更新系命名":
			reasons = append(reasons, "更新系命名：UPDATE 时前进，水位语义最佳")
		case "创建系命名":
			reasons = append(reasons, "创建系命名：可与更新系混淆，已降权")
			warnings = append(warnings, "创建系列仅在 INSERT 时前进，UPDATE 的行不会被捕获")
		case "自增系命名":
			reasons = append(reasons, "自增系命名：仅作末位候选")
			warnings = append(warnings, "自增/ID 列对 UPDATE 完全不敏感，漏更新行")
		default:
			reasons = append(reasons, "非字典命名：按覆盖度/类型/索引参与评分")
		}
		if a.anyDate {
			warnings = append(warnings, "date 类型为天粒度，同日多次变更可能漏行")
		}
		if idxRatio < 1 {
			warnings = append(warnings, "部分匹配表该列无索引，增量扫描可能全表扫")
		}
		if nowRatio > 0 {
			reasons = append(reasons, "带 DEFAULT now()/CURRENT_TIMESTAMP：自动维护信号")
		}
		if cov < 1 {
			reasons = append(reasons, "未覆盖全表（缺列表见 unmatched_tables）")
		}

		matched := append([]string{}, a.matchedTables...)
		sort.Strings(matched)
		unmatched := []string{}
		for t := range tables {
			found := false
			for _, m := range matched {
				if m == t {
					found = true
					break
				}
			}
			if !found {
				unmatched = append(unmatched, t)
			}
		}
		sort.Strings(unmatched)

		cands = append(cands, wmSuggestCandidate{
			Column: name, Score: score, Coverage: cov,
			IndexedRatio: idxRatio, DefaultNowRatio: nowRatio,
			TypeHistogram: a.hist, MatchedTables: matched,
			UnmatchedTables: unmatched, Reasons: reasons, Warnings: warnings,
		})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].Score != cands[j].Score {
			return cands[i].Score > cands[j].Score
		}
		return cands[i].Column < cands[j].Column
	})
	if len(cands) > 5 {
		cands = cands[:5]
	}
	return total, cands
}

// handleSuggestWatermark: POST /incremental/suggest-watermark {source_ref}
// → top-5 unified watermark column candidates for the whole source schema.
func (s *Server) handleSuggestWatermark(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SourceRef string `json:"source_ref"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.SourceRef == "" {
		s.writeError(w, http.StatusBadRequest, "source_ref is required")
		return
	}
	e, err := s.resolveDataSourceRef(req.SourceRef)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "source_ref: "+err.Error())
		return
	}
	if e.Type != "postgres" {
		s.writeError(w, http.StatusBadRequest, "增量同步 v1 仅支持 PostgreSQL 源数据源")
		return
	}
	sc := dataSourceToSourceConfig(e)
	if wmSystemSchemas[sc.Schema] {
		s.writeError(w, http.StatusBadRequest, "系统 schema 不参与水位建议")
		return
	}
	db, err := openPGTestConn(sc.DSN())
	if err != nil {
		s.writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	cols, err := queryWMCatalog(ctx, db, sc.Schema)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "catalog query failed: "+err.Error())
		return
	}
	total, cands := scoreWatermarkCandidates(cols)
	if cands == nil {
		cands = []wmSuggestCandidate{}
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_tables": total,
		"candidates":   cands,
	})
}
