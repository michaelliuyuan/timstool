// compare_multidb.go implements the A2 multi-database batch task (#102):
// one compare task covering N databases (explicit names and/or wildcard
// patterns with an exclusion list), the engine looping the databases
// SEQUENTIALLY through the unchanged single-database flow (no new
// concurrency surface — every per-database run owns its own pools and
// closes them, exactly like a legacy task), progress upgraded to
// 「X/Y 库 · 表 done/total」, and a cross-database summary written at the
// end (summary.json + summary.csv + the report face's html rendering).
//
// Compatibility red lines (task #102):
//   - a task without `databases` runs the legacy single-database path
//     byte-identically (all new task.json fields are omitempty);
//   - keyset/timeout/heartbeat semantics are untouched — the per-database
//     run is the very same Validator.RunWithDSNs call the legacy path
//     makes, just in a loop with per-database Source/Target.Database.
package webapi

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/common/reporter"
	"github.com/michaelliuyuan/timstool/internal/source"
	"github.com/michaelliuyuan/timstool/internal/source/mysql"
	"github.com/michaelliuyuan/timstool/internal/validator"
	"go.uber.org/zap"
)

// Per-database result statuses. pass/warn/fail mirror the engine's report
// vocabulary (a database whose compare produced evidence); error marks a
// database that could not run at all (connect failure, all-tables-skip
// watermark guard, …); cancelled marks the database under the cursor when
// the task was cancelled mid-loop.
const (
	compareDBStatusPass      = "pass"
	compareDBStatusWarn      = "warn"
	compareDBStatusFail      = "fail"
	compareDBStatusError     = "error"
	compareDBStatusCancelled = "cancelled"
)

// Caps for the multi-db selection (#102). The request-side cap keeps a
// fat-fingered paste from queuing hundreds of entries; the resolved cap
// guards a `*` pattern against a server with an extreme database count —
// both fail loud with a named limit, never a silent truncation.
const (
	maxCompareDatabasesRequest = 64
	maxCompareDatabasesRun     = 256
)

// compareDBPatternRe is the intake allow-list for database names and
// wildcard patterns: letters, digits, underscore, dollar, dash, plus the
// two wildcard chars `*` and `?`. These strings never enter SQL text (they
// only steer DSN assembly via the existing escaping builders and
// server-generated filesystem slugs), so the allow-list is a shape guard,
// not the injection defense — which stays where it always was.
var compareDBPatternRe = regexp.MustCompile(`^[A-Za-z0-9_$?*-]+$`)

// compareDBSlugSanitizeRe strips everything outside the filesystem-safe
// alphabet for per-database subdirectory names.
var compareDBSlugSanitizeRe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// hasWildcard reports whether a selection entry needs server-side
// resolution (contains `*` or `?`).
func hasCompareDBWildcard(s string) bool {
	return strings.ContainsAny(s, "*?")
}

// normalizeCompareDBList validates and canonicalizes a multi-db selection
// or exclusion list: entries trimmed, empties dropped, exact duplicates
// collapsed (first wins), every entry against the pattern allow-list,
// capped at maxCompareDatabasesRequest. nil/empty stays nil.
func normalizeCompareDBList(in []string, field string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, e := range in {
		t := strings.TrimSpace(e)
		if t == "" {
			continue
		}
		if !compareDBPatternRe.MatchString(t) {
			return nil, fmt.Errorf("%s: 非法库名或通配模式 %q（仅允许字母、数字、_、$、- 与通配符 * ?）", field, t)
		}
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) > maxCompareDatabasesRequest {
		return nil, fmt.Errorf("%s: 最多 %d 个条目（当前 %d）", field, maxCompareDatabasesRequest, len(out))
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// compareDBPatternMatch matches one name against one selection entry.
// Entries without wildcards compare exactly (case-sensitive, the identity
// both PG and MySQL operators work with); `*` and `?` translate to their
// glob meaning, everything else is literal.
func compareDBPatternMatch(pattern, name string) bool {
	if !hasCompareDBWildcard(pattern) {
		return pattern == name
	}
	var sb strings.Builder
	sb.WriteString(`^`)
	for _, r := range pattern {
		switch r {
		case '*':
			sb.WriteString(`.*`)
		case '?':
			sb.WriteString(`.`)
		default:
			sb.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	sb.WriteString(`$`)
	re, err := regexp.Compile(sb.String())
	if err != nil {
		return false
	}
	return re.MatchString(name)
}

// compareListSourceDatabases lists the user-visible databases on the source
// server for wildcard resolution. It is a package var (the
// compareHeartbeatTick seam pattern) so unit tests can fake the listing
// without a live server. The walk dispatches on the registry predicate
// source.IsMySQLWire (the SHOW DATABASES dialect family — the same routing
// class as dbconn.go's sourceConnSpec and ddlexport's ListSchemasFor, so
// no raw type branch joins the frozen baseline): MySQL-wire sources walk
// SHOW DATABASES minus the shared system-schema set; everything else (PG)
// walks pg_database (connectable, non-template).
var compareListSourceDatabases = func(ctx context.Context, sc config.SourceConfig) ([]string, error) {
	if source.IsMySQLWire(sc.SourceType()) {
		db, err := sql.Open("mysql", config.BuildMySQLDSN(sc.Host, sc.Port, sc.User, sc.Password, "", map[string]string{"charset": "utf8mb4"}, nil))
		if err != nil {
			return nil, fmt.Errorf("list databases: %w", err)
		}
		defer db.Close()
		rows, err := db.QueryContext(ctx, `SHOW DATABASES`)
		if err != nil {
			return nil, fmt.Errorf("list databases (SHOW DATABASES): %w", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return nil, err
			}
			if mysql.SystemDatabases[strings.ToLower(name)] {
				continue
			}
			out = append(out, name)
		}
		return out, rows.Err()
	}
	// PG: the listing connection needs SOME database; the task's configured
	// database is the natural maintenance handle, "postgres" the fallback.
	// The pgx/mysql drivers are already blank-imported by package webapi
	// (dbconn.go).
	maint := sc.Database
	if maint == "" {
		maint = "postgres"
	}
	db, err := sql.Open("pgx", config.BuildPGDSN(sc.Host, sc.Port, sc.User, sc.Password, maint, sc.SSLMode, nil))
	if err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `
SELECT datname FROM pg_database
WHERE datallowconn AND NOT datistemplate
ORDER BY datname`)
	if err != nil {
		return nil, fmt.Errorf("list databases (pg_database): %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// resolveCompareDatabases turns the task's selection (literals and/or
// wildcards, minus exclusions) into the concrete ordered database list the
// run loop walks. Literals pass through untouched (the operator named
// them; a stale name fails loudly at that database's connect step);
// wildcards expand against the live server listing. The result is sorted
// (deterministic progress/restart ordering) and deduplicated.
func resolveCompareDatabases(ctx context.Context, sc config.SourceConfig, selection, excludes []string) ([]string, error) {
	set := make(map[string]bool)
	needListing := false
	for _, p := range selection {
		if hasCompareDBWildcard(p) {
			needListing = true
			break
		}
	}
	if needListing {
		listing, err := compareListSourceDatabases(ctx, sc)
		if err != nil {
			return nil, fmt.Errorf("解析多库通配模式失败: %w", err)
		}
		for _, p := range selection {
			if !hasCompareDBWildcard(p) {
				continue
			}
			for _, name := range listing {
				if compareDBPatternMatch(p, name) {
					set[name] = true
				}
			}
		}
	}
	for _, p := range selection {
		if !hasCompareDBWildcard(p) {
			set[p] = true
		}
	}
	for _, x := range excludes {
		for name := range set {
			if compareDBPatternMatch(x, name) {
				delete(set, name)
			}
		}
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("多库选择未解析到任何数据库（选择 %v，排除 %v）", selection, excludes)
	}
	if len(set) > maxCompareDatabasesRun {
		return nil, fmt.Errorf("多库解析结果 %d 个超出上限 %d，请收窄选择或增加排除项", len(set), maxCompareDatabasesRun)
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// compareDBSlug builds the per-database subdirectory name under
// compare/{id}/db/: the run ordinal keeps names unique after sanitization
// and gives the directory listing a stable run order.
func compareDBSlug(i int, name string) string {
	slug := compareDBSlugSanitizeRe.ReplaceAllString(name, "_")
	if len(slug) > 64 {
		slug = slug[:64]
	}
	if slug == "" {
		slug = "db"
	}
	return fmt.Sprintf("%02d-%s", i+1, slug)
}

// CompareDatabaseResult is one database's row in the cross-database
// summary (persisted on the task and in summary.json; the same shape rides
// the WS progress payload's terminal broadcast via task.database_results).
type CompareDatabaseResult struct {
	Database    string `json:"database"`
	Status      string `json:"status"` // pass | warn | fail | error | cancelled
	TotalTables int    `json:"total_tables"`
	PassTables  int    `json:"pass_tables"`
	FailTables  int    `json:"fail_tables"`
	WarnTables  int    `json:"warn_tables"`
	SkipTables  int    `json:"skip_tables"`
	DiffRows    int64  `json:"diff_rows"`
	Duration    string `json:"duration,omitempty"`
	Error       string `json:"error,omitempty"`
}

// classifyPerDBOutcome turns one finished per-database run into its summary
// row. It is the single place that decides what a returned report MEANS,
// kept as a pure function so the anchors read the production logic instead
// of re-implementing it (the M1 false-anchor lesson).
//
// Order matters and mirrors the legacy single-db flow:
//  1. a watermark-active run that skipped every table produced no
//     evidence — error (the legacy compareAllTablesSkipped guard);
//  2. the engine downgrades a failed reporter.Save to a Warn and hands
//     back a healthy in-memory report (validator.go:624-626), so the
//     artifact on disk is the only proof the ?db= face will work. A
//     missing/unparsable report.json is an error — 红队 B1's real
//     failure mode, one layer deeper than the missing directory;
//  3. otherwise the in-memory report is trustworthy.
func classifyPerDBOutcome(db string, report *reporter.Report, reportFile string, task *CompareTask) (CompareDatabaseResult, bool) {
	if (task.Watermark != nil || len(task.TableWatermarks) > 0) && compareAllTablesSkipped(report) {
		return CompareDatabaseResult{
			Database: db, Status: compareDBStatusError,
			Error: "所有表均被跳过（无可比列或列全被排除），未产生比对结果",
		}, true
	}
	if artErr := verifyPerDBReport(reportFile); artErr != nil {
		return CompareDatabaseResult{Database: db, Status: compareDBStatusError, Error: artErr.Error()}, true
	}
	return compareDBResultFromReport(db, report), false
}

// verifyPerDBReport confirms the per-database report artifact really exists
// and really parses. The engine (validator.go:624-626) downgrades a failed
// reporter.Save to a Warn and hands back the in-memory report, so without
// this check a full/read-only/permission-broken data dir yields a green
// task whose ?db= face 404s.
func verifyPerDBReport(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("库报告未落盘（%s）: %w", path, err)
	}
	var rpt reporter.Report
	if err := json.Unmarshal(raw, &rpt); err != nil {
		return fmt.Errorf("库报告落盘但不可解析（%s）: %w", path, err)
	}
	return nil
}

// compareDBResultFromReport summarizes one finished per-database run.
func compareDBResultFromReport(db string, rpt *reporter.Report) CompareDatabaseResult {
	r := CompareDatabaseResult{
		Database:    db,
		Status:      string(rpt.Status),
		TotalTables: rpt.Stats.TotalTables,
		PassTables:  rpt.Stats.PassTables,
		FailTables:  rpt.Stats.FailTables,
		WarnTables:  rpt.Stats.WarnTables,
		SkipTables:  rpt.Stats.SkipTables,
		DiffRows:    rpt.Stats.TotalDiffRows,
		Duration:    rpt.Duration,
	}
	if r.Status == "" && rpt.EndTime.After(rpt.StartTime) {
		r.Status = string(reporter.StatusPass)
	}
	return r
}

// compareDBStatusRank orders summary statuses worst-first for the overall
// verdict: error > cancelled > fail > warn > pass.
func compareDBStatusRank(s string) int {
	switch s {
	case compareDBStatusError:
		return 0
	case compareDBStatusCancelled:
		return 1
	case compareDBStatusFail:
		return 2
	case compareDBStatusWarn:
		return 3
	default:
		return 4
	}
}

// compareSummaryOverallStatus derives the cross-database verdict from the
// per-database rows (error/cancelled poison it, then fail, then warn).
func compareSummaryOverallStatus(results []CompareDatabaseResult) string {
	if len(results) == 0 {
		// 红队 m4: an empty summary only exists for a run that ended
		// before the first database produced a row — reading "pass"
		// would mislead; fall back to cancelled.
		return compareDBStatusCancelled
	}
	worst := compareDBStatusPass
	for _, r := range results {
		if compareDBStatusRank(r.Status) < compareDBStatusRank(worst) {
			worst = r.Status
		}
	}
	return worst
}

// CompareSummaryTotals aggregates the per-database rows for the summary
// header card (and the CSV totals row).
type CompareSummaryTotals struct {
	Databases     int   `json:"databases"`
	DatabasesBad  int   `json:"databases_bad"` // fail + error + cancelled
	DatabasesPass int   `json:"databases_pass"`
	TotalTables   int   `json:"total_tables"`
	PassTables    int   `json:"pass_tables"`
	FailTables    int   `json:"fail_tables"`
	WarnTables    int   `json:"warn_tables"`
	SkipTables    int   `json:"skip_tables"`
	TotalDiffRows int64 `json:"total_diff_rows"`
}

// CompareSummary is the persisted shape of compare/{id}/summary.json: the
// cross-database roll-up of a finished (or partially finished) multi-db
// task — one row per database plus totals.
type CompareSummary struct {
	TaskID     string                  `json:"task_id"`
	TaskName   string                  `json:"task_name,omitempty"`
	Mode       string                  `json:"mode,omitempty"`
	Source     string                  `json:"source,omitempty"` // host:port (no credentials)
	Target     string                  `json:"target,omitempty"`
	Status     string                  `json:"status"`
	StartedAt  time.Time               `json:"started_at"`
	FinishedAt time.Time               `json:"finished_at"`
	Duration   string                  `json:"duration,omitempty"`
	Databases  []CompareDatabaseResult `json:"databases"`
	Totals     CompareSummaryTotals    `json:"totals"`
	Note       string                  `json:"note,omitempty"` // partial-run disclosure (cancelled/failed mid-loop)
}

// buildCompareSummary assembles the summary from the per-database results.
func buildCompareSummary(task *CompareTask, results []CompareDatabaseResult, started, finished time.Time, note string) *CompareSummary {
	sum := &CompareSummary{
		TaskID:     task.ID,
		TaskName:   task.Name,
		Mode:       task.Mode,
		Source:     dcEndpointLabel(task.Source.Host, task.Source.Port, ""),
		Target:     dcEndpointLabel(task.Target.Host, task.Target.Port, ""),
		Status:     compareSummaryOverallStatus(results),
		StartedAt:  started,
		FinishedAt: finished,
		Duration:   reporter.FormatDuration(finished.Sub(started)),
		Databases:  results,
		Note:       note,
	}
	for _, r := range results {
		sum.Totals.Databases++
		switch r.Status {
		case compareDBStatusPass, compareDBStatusWarn:
			sum.Totals.DatabasesPass++
		default:
			sum.Totals.DatabasesBad++
		}
		sum.Totals.TotalTables += r.TotalTables
		sum.Totals.PassTables += r.PassTables
		sum.Totals.FailTables += r.FailTables
		sum.Totals.WarnTables += r.WarnTables
		sum.Totals.SkipTables += r.SkipTables
		sum.Totals.TotalDiffRows += r.DiffRows
	}
	return sum
}

// writeCompareSummary persists summary.json + summary.csv atomically
// (temp+rename, the persistCompareTask pattern). A summary write failure is
// returned to the caller — a multi-db task whose summary cannot land must
// not read as completed.
func writeCompareSummary(dir string, sum *CompareSummary) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWriteFile(filepath.Join(dir, "summary.json"), raw); err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(dir, "summary.csv"), compareSummaryCSV(sum))
}

// atomicWriteFile is the shared temp+rename write used by the compare
// artifacts (task.json pattern).
func atomicWriteFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "cmpsum-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

// compareCSVSafeCell neutralizes CSV formula injection (红队 m2): Excel
// and friends interpret a leading = + - @ as a formula, so text cells get
// a leading apostrophe. The numeric columns are exempt — a negative count
// must stay a number.
func compareCSVSafeCell(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@':
		return "'" + s
	}
	return s
}

// compareSummaryCSV renders the 汇总 CSV: one row per database plus a
// totals row. UTF-8 BOM + CRLF so Excel opens the Chinese headers
// correctly (the FE exportCsv precedent); every cell passes through the
// csv writer's quoting, text cells through compareCSVSafeCell.
func compareSummaryCSV(sum *CompareSummary) []byte {
	var b strings.Builder
	b.WriteString("\uFEFF")
	w := csv.NewWriter(&b)
	_ = w.Write([]string{"库名", "状态", "表总数", "通过", "失败", "警告", "跳过", "差异行数", "耗时", "错误"})
	for _, r := range sum.Databases {
		_ = w.Write([]string{
			compareCSVSafeCell(r.Database), compareCSVSafeCell(r.Status),
			fmt.Sprintf("%d", r.TotalTables),
			fmt.Sprintf("%d", r.PassTables),
			fmt.Sprintf("%d", r.FailTables),
			fmt.Sprintf("%d", r.WarnTables),
			fmt.Sprintf("%d", r.SkipTables),
			fmt.Sprintf("%d", r.DiffRows),
			compareCSVSafeCell(r.Duration),
			compareCSVSafeCell(r.Error),
		})
	}
	_ = w.Write([]string{
		compareCSVSafeCell("合计（" + fmt.Sprintf("%d", sum.Totals.Databases) + " 库）"), compareCSVSafeCell(sum.Status),
		fmt.Sprintf("%d", sum.Totals.TotalTables),
		fmt.Sprintf("%d", sum.Totals.PassTables),
		fmt.Sprintf("%d", sum.Totals.FailTables),
		fmt.Sprintf("%d", sum.Totals.WarnTables),
		fmt.Sprintf("%d", sum.Totals.SkipTables),
		fmt.Sprintf("%d", sum.Totals.TotalDiffRows),
		compareCSVSafeCell(sum.Duration),
		"",
	})
	w.Flush()
	return []byte(strings.ReplaceAll(b.String(), "\n", "\r\n"))
}

// runCompareMultiDB is the A2 orchestration loop: resolve the selection
// into the concrete database list, run each database through the unchanged
// single-database engine (sequentially — the single-flight slot and the
// concurrency budget mean what they always meant), collect one
// CompareDatabaseResult per database, and write the cross-database summary.
// Lifecycle notes:
//   - a database that cannot run (connect failure, watermark all-skip
//     guard) is recorded as status=error and the loop CONTINUES — the
//     summary must still cover the remaining databases;
//   - the task fails (status=failed) when any database errored, with the
//     error naming them — a partial infrastructure failure is a task-level
//     error, never a green 「completed」;
//   - cancellation stops the loop: the in-flight database is recorded as
//     cancelled, the partial summary is still written (note discloses it),
//     and the task settles as cancelled;
//   - a summary that cannot land fails the task too (multiDBFinishDecision)
//     — the summary faces are part of the deliverable, not a bonus.
func (s *Server) runCompareMultiDB(ctx context.Context, task *CompareTask, taskMu *sync.Mutex, finish func(status, errMsg string), logger *zap.Logger) {
	started := time.Now()

	resolved, err := resolveCompareDatabases(ctx, task.Source, task.Databases, task.ExcludeDatabases)
	taskMu.Lock()
	if err != nil {
		taskMu.Unlock()
		logger.Warn("multi-db resolution failed", zap.String("compare_id", task.ID), zap.Error(err))
		finish(CompareStatusFailed, err.Error())
		return
	}
	task.ResolvedDatabases = resolved
	task.DatabasesTotal = len(resolved)
	task.DatabasesDone = 0
	task.LastProgressAt = time.Now()
	if perr := s.persistCompareTask(task); perr != nil {
		logger.Warn("failed to persist multi-db resolution", zap.Error(perr))
	}
	s.BroadcastProgress(task.ID, map[string]interface{}{
		"type": "compare", "status": task.Status,
		"databases_done": 0, "databases_total": len(resolved),
	})
	taskMu.Unlock()

	results := make([]CompareDatabaseResult, 0, len(resolved))
	var errDBs []string
	for i, db := range resolved {
		if ctx.Err() != nil {
			results = append(results, CompareDatabaseResult{Database: db, Status: compareDBStatusCancelled, Error: "cancelled"})
			errDBs = append(errDBs, db)
			break
		}
		taskMu.Lock()
		task.CurrentDatabase = db
		task.TablesDone = 0
		task.TablesTotal = 0
		task.CurrentTable = ""
		task.CurrentChunk = 0
		task.TotalChunks = 0
		task.LastProgressAt = time.Now()
		if perr := s.persistCompareTask(task); perr != nil {
			logger.Warn("failed to persist multi-db progress", zap.Error(perr))
		}
		s.BroadcastProgress(task.ID, map[string]interface{}{
			"type": "compare", "status": task.Status,
			"databases_done": i, "databases_total": len(resolved), "current_database": db,
		})
		taskMu.Unlock()

		src := task.Source
		src.Database = db
		tgt := task.Target
		tgt.Database = db
		slug := compareDBSlug(i, db)
		dbDir := filepath.Join(s.compareDir(task.ID), "db", slug)
		// 红队 B1: the engine writes report.json via reporter.Save's
		// os.CreateTemp(dir), which needs the per-db directory to EXIST —
		// nothing else creates it (the engine self-creates only the diff/
		// and spool/ roots), and a failed save is swallowed as a Warn
		// inside the validator, leaving a green task with a 404 ?db= face.
		// A mkdir failure is honored like a connect failure: error row,
		// loop continues.
		var report *reporter.Report
		var rerr error
		reportFile := filepath.Join(dbDir, "report.json")
		if mkErr := os.MkdirAll(dbDir, 0o755); mkErr != nil {
			rerr = fmt.Errorf("创建库报告目录失败: %w", mkErr)
		} else {
			report, rerr = s.executeCompareRun(ctx, task, taskMu, src, tgt,
				reportFile,
				filepath.Join(s.compareDir(task.ID), "diff", slug),
				filepath.Join(s.dataDir, "spool", task.ID, slug))
		}
		// B1 (the legacy rule): check ctx FIRST regardless of rerr — Run
		// swallows per-table cancellations into failed table reports.
		if ctx.Err() != nil {
			results = append(results, CompareDatabaseResult{Database: db, Status: compareDBStatusCancelled, Error: "cancelled"})
			errDBs = append(errDBs, db)
			break
		}
		if rerr != nil {
			results = append(results, CompareDatabaseResult{Database: db, Status: compareDBStatusError, Error: rerr.Error()})
			errDBs = append(errDBs, db)
		} else if row, rowBad := classifyPerDBOutcome(db, report, reportFile, task); rowBad {
			results = append(results, row)
			errDBs = append(errDBs, db)
		} else {
			results = append(results, row)
		}

		taskMu.Lock()
		task.DatabasesDone = i + 1
		task.LastProgressAt = time.Now()
		if perr := s.persistCompareTask(task); perr != nil {
			logger.Warn("failed to persist multi-db step", zap.Error(perr))
		}
		taskMu.Unlock()
	}

	cancelled := ctx.Err() != nil
	note := ""
	if cancelled {
		note = fmt.Sprintf("任务取消：已覆盖 %d/%d 个库", len(results), len(resolved))
	}
	sum := buildCompareSummary(task, results, started, time.Now(), note)
	sumErr := ""
	if serr := writeCompareSummary(s.compareDir(task.ID), sum); serr != nil {
		logger.Warn("failed to write multi-db summary", zap.String("compare_id", task.ID), zap.Error(serr))
		sumErr = fmt.Sprintf("汇总报告写盘失败: %v", serr)
	}

	taskMu.Lock()
	task.DatabaseResults = results
	taskMu.Unlock()

	status, msg := multiDBFinishDecision(cancelled, errDBs, sumErr, len(resolved))
	if status == CompareStatusCompleted {
		logger.Info("multi-db comparison finished",
			zap.String("compare_id", task.ID),
			zap.String("overall", sum.Status),
			zap.Int("databases", len(results)))
	}
	finish(status, msg)
}

// multiDBFinishDecision is the loop's terminal verdict as a pure function
// (table-tested): cancellation wins; else any failed database fails the
// task naming them; else a summary write failure ALSO fails the task (红队
// M2 — writeCompareSummary's contract comment says a summary that cannot
// land must not read as completed; before this the loop only Warn'd and
// finished green); only a clean sweep completes.
func multiDBFinishDecision(cancelled bool, errDBs []string, sumErr string, resolved int) (string, string) {
	switch {
	case cancelled:
		if sumErr != "" {
			return CompareStatusCancelled, "cancelled；" + sumErr
		}
		return CompareStatusCancelled, "cancelled"
	case len(errDBs) > 0:
		msg := fmt.Sprintf("%d/%d 个库执行失败：%s", len(errDBs), resolved, strings.Join(errDBs, ", "))
		if sumErr != "" {
			msg += "；" + sumErr
		}
		return CompareStatusFailed, msg
	case sumErr != "":
		return CompareStatusFailed, sumErr
	default:
		return CompareStatusCompleted, ""
	}
}

// executeCompareRun is the single-database engine call shared by the legacy
// path and the A2 loop: build the validator for THIS source/target pair
// (the multi-db loop passes per-database Database overrides), wire the
// progress callbacks (db-aware on the multi-db task), and run
// RunWithDSNs. Everything below is the legacy runCompare body, lifted
// verbatim except for the parameterization — keyset/timeout/heartbeat
// semantics untouched (#102 red line).
func (s *Server) executeCompareRun(ctx context.Context, task *CompareTask, taskMu *sync.Mutex,
	src config.SourceConfig, tgt config.TargetConfig, reportFile, diffDir, spoolRoot string) (*reporter.Report, error) {

	logger := zap.L()
	multi := task.DatabasesTotal > 0

	// MS-08: the source TYPE must travel with the config so NewValidator
	// assembles the right source dialect/driver.
	v := validator.NewValidator(config.Config{Source: src})
	v.OnTableDone(func(done, total int, tr reporter.TableReport) {
		taskMu.Lock()
		defer taskMu.Unlock()
		task.TablesDone = done
		task.TablesTotal = total
		task.CurrentTable = tr.TableName
		// #t95 P1: every progress event is a heartbeat.
		task.PossiblyStuck = false
		task.LastProgressAt = time.Now()
		if err := s.persistCompareTask(task); err != nil {
			logger.Warn("failed to persist compare progress", zap.Error(err))
		}
		payload := tableDoneProgress(task, done, total, tr)
		if multi {
			payload["databases_done"] = task.DatabasesDone
			payload["databases_total"] = task.DatabasesTotal
			payload["current_database"] = task.CurrentDatabase
		}
		s.BroadcastProgress(task.ID, payload)
	})
	// #t95 P1: chunk-level progress (checksum mode).
	v.OnChunkProgress(func(table string, idx, total int) {
		taskMu.Lock()
		defer taskMu.Unlock()
		task.CurrentTable = table
		task.CurrentChunk = idx + 1
		task.TotalChunks = total
		task.PossiblyStuck = false
		task.LastProgressAt = time.Now()
		if err := s.persistCompareTask(task); err != nil {
			logger.Warn("failed to persist compare chunk progress", zap.Error(err))
		}
		payload := map[string]interface{}{
			"type": "compare", "status": task.Status,
			"current_table": table, "current_chunk": task.CurrentChunk, "total_chunks": task.TotalChunks,
			"possibly_stuck": task.PossiblyStuck,
		}
		if multi {
			payload["databases_done"] = task.DatabasesDone
			payload["databases_total"] = task.DatabasesTotal
			payload["current_database"] = task.CurrentDatabase
		}
		s.BroadcastProgress(task.ID, payload)
	})

	_, validateOpts, compareCfg := buildCompareRunParams(task, reportFile)
	// BUG-CMP-COUNT-P1 A6: inherit ONLY the two chunked-count knobs from
	// the live config.yaml snapshot (never the zero value = disabled).
	inheritChunkedCountKnobs(&compareCfg, s.cdcCfgSnapshot, logger)
	validateOpts.DiffDetailDir = diffDir
	validateOpts.SpoolRoot = spoolRoot
	// MS-08: DSNByType dispatches on the source type.
	return v.RunWithDSNs(ctx, src.DSNByType(), tgt.DSN(), src.Schema, compareCfg, task.Parallel, validateOpts)
}

// compareDBStatusCN maps the per-database statuses onto the report
// vocabulary (dcStatusCN twin extended with the multi-db-only states).
func compareDBStatusCN(s string) string {
	switch s {
	case compareDBStatusError:
		return "执行失败"
	case compareDBStatusCancelled:
		return "已取消"
	default:
		return dcStatusCN(s)
	}
}

// renderCompareSummaryHTML builds the A2 cross-database summary report in
// the family format (renderDataCompareReportHTML twin: TiMS banner + meta
// grid, overview/stats cards, one row per database — 库名/状态/表数/差异行
// 数/耗时/错误, worst-first sort with the original # kept). All dynamic
// strings pass mcHTMLEsc; numbers are comma-formatted (dcComma).
func renderCompareSummaryHTML(task *CompareTask, sum *CompareSummary) string {
	var sb strings.Builder
	sb.WriteString(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>TiMS 数据比对汇总报告</title>
<style>
` + reporter.ReportCSS + `
.header { background: linear-gradient(135deg, #0C1222, #131B30); color: #EEF1F8; padding: 32px; border-radius: 12px; margin-bottom: 24px; border: 1px solid #263154; }
.header h1 { font-size: 28px; margin-bottom: 8px; }
.header .logo { color: #E13C3C; font-weight: 900; }
.header .subtitle { color: #97A0B5; font-size: 14px; }
.header .meta { margin-top: 16px; display: grid; grid-template-columns: repeat(auto-fit, minmax(260px, 1fr)); gap: 4px 24px; font-size: 13px; }
.header .meta div { color: #97A0B5; }
.header .meta span { color: #EEF1F8; font-weight: 500; }
th { white-space: nowrap; }
td.num { white-space: nowrap; }
.badge { white-space: nowrap; }
td:not(.num) { overflow-wrap: anywhere; }
th, td { padding: 8px 10px; }
td.num, th.colnum { text-align: right; }
td.diff-pos { color: #E13C3C; font-weight: 600; }
thead th { position: sticky; top: 0; z-index: 5; }
tr.row-fail td { background: #FDF1F1; }
tr.row-fail td:first-child { box-shadow: inset 3px 0 0 #E13C3C; }
.tims-version { font-size: 11px; opacity: 0.85; }
@media print { thead th { position: static; } }
</style>
</head>
<body>
<div class="container">
<div class="header">
<h1><span class="logo">Ti</span>MS 数据比对汇总报告（多库）</h1>
<div class="subtitle">`)
	sb.WriteString(mcHTMLEsc(sum.StartedAt.Format("2006-01-02 15:04:05")))
	sb.WriteString(` ~ `)
	sb.WriteString(mcHTMLEsc(sum.FinishedAt.Format("2006-01-02 15:04:05")))
	sb.WriteString(`</div>
<div class="meta">`)
	fmt.Fprintf(&sb, `<div>任务 ID <span class="num">%s</span></div>`, mcHTMLEsc(task.ID))
	if sum.TaskName != "" {
		fmt.Fprintf(&sb, `<div>任务名 <span>%s</span></div>`, mcHTMLEsc(sum.TaskName))
	}
	fmt.Fprintf(&sb, `<div>源端 <span>%s</span></div>`, mcHTMLEsc(dcEndpointLabel(task.Source.Host, task.Source.Port, strings.Join(task.Databases, ", "))))
	fmt.Fprintf(&sb, `<div>目标 <span>%s</span></div>`, mcHTMLEsc(dcEndpointLabel(task.Target.Host, task.Target.Port, "同名库")))
	fmt.Fprintf(&sb, `<div>比对模式 <span>%s</span></div>`, mcHTMLEsc(dcModeLabel(task)))
	fmt.Fprintf(&sb, `<div>耗时 <span class="num">%s</span></div>`, mcHTMLEsc(sum.Duration))
	sb.WriteString(`</div></div>`)

	statusClass := "overall-" + sum.Status
	sb.WriteString(`<div class="card"><h2>概览</h2><div class="stats">`)
	fmt.Fprintf(&sb, `<div class="stat"><div class="value %s">%s</div><div class="label">总体状态</div></div>`, mcHTMLEsc(statusClass), mcHTMLEsc(compareDBStatusCN(sum.Status)))
	fmt.Fprintf(&sb, `<div class="stat"><div class="value">%d</div><div class="label">库总数</div></div>`, sum.Totals.Databases)
	fmt.Fprintf(&sb, `<div class="stat"><div class="value c-teal">%d</div><div class="label">通过库数</div></div>`, sum.Totals.DatabasesPass)
	fmt.Fprintf(&sb, `<div class="stat"><div class="value c-brand">%d</div><div class="label">失败/异常库数</div></div>`, sum.Totals.DatabasesBad)
	fmt.Fprintf(&sb, `<div class="stat"><div class="value">%d</div><div class="label">总表数</div></div>`, sum.Totals.TotalTables)
	fmt.Fprintf(&sb, `<div class="stat"><div class="value">%s</div><div class="label">差异总行数</div></div>`, mcHTMLEsc(dcComma(sum.Totals.TotalDiffRows)))
	sb.WriteString(`</div>`)
	if sum.Note != "" {
		fmt.Fprintf(&sb, `<div class="summary">%s</div>`, mcHTMLEsc(sum.Note))
	}
	sb.WriteString(`</div>`)

	sb.WriteString(`<div class="card"><h2>统计</h2><div class="stats">`)
	fmt.Fprintf(&sb, `<div class="stat"><div class="value c-teal">%d</div><div class="label">表通过</div></div>`, sum.Totals.PassTables)
	fmt.Fprintf(&sb, `<div class="stat"><div class="value c-brand">%d</div><div class="label">表失败</div></div>`, sum.Totals.FailTables)
	fmt.Fprintf(&sb, `<div class="stat"><div class="value c-amber">%d</div><div class="label">表警告</div></div>`, sum.Totals.WarnTables)
	fmt.Fprintf(&sb, `<div class="stat"><div class="value c-muted">%d</div><div class="label">表跳过</div></div>`, sum.Totals.SkipTables)
	sb.WriteString(`</div></div>`)

	if len(sum.Databases) > 0 {
		sb.WriteString(`<div class="card"><h2>各库结果</h2><table><thead><tr>`)
		sb.WriteString(`<th>#</th><th>库名</th><th>状态</th><th class="colnum">表总数</th><th class="colnum">通过</th><th class="colnum">失败</th><th class="colnum">警告</th><th class="colnum">跳过</th><th class="colnum">差异行数</th><th class="colnum">耗时</th><th>错误</th>`)
		sb.WriteString(`</tr></thead><tbody>`)
		order := make([]int, len(sum.Databases))
		for i := range order {
			order[i] = i
		}
		sort.SliceStable(order, func(a, b int) bool {
			return compareDBStatusRank(sum.Databases[order[a]].Status) < compareDBStatusRank(sum.Databases[order[b]].Status)
		})
		for _, idx := range order {
			r := sum.Databases[idx]
			rowClass := ""
			switch r.Status {
			case compareDBStatusFail, compareDBStatusError:
				rowClass = ` class="row-fail"`
			}
			badgeClass := "badge-" + r.Status
			diffHTML := `<td class="num">—</td>`
			if r.DiffRows != 0 {
				diffHTML = fmt.Sprintf(`<td class="num diff-pos">%s</td>`, mcHTMLEsc(dcComma(r.DiffRows)))
			}
			durHTML := `<td class="num">—</td>`
			if r.Duration != "" {
				durHTML = fmt.Sprintf(`<td class="num">%s</td>`, mcHTMLEsc(r.Duration))
			}
			// 红队 m1: truncate at a RUNE boundary on the raw string — byte-
			// slicing the escaped text halves multi-byte characters (and can
			// cut an HTML entity in half) — then escape.
			errStr := r.Error
			if runes := []rune(errStr); len(runes) > 80 {
				errStr = string(runes[:80]) + "..."
			}
			errStr = mcHTMLEsc(errStr)
			fmt.Fprintf(&sb,
				`<tr%s><td class="num">%d</td><td>%s</td><td><span class="badge %s">%s</span></td><td class="num">%d</td><td class="num">%d</td><td class="num">%d</td><td class="num">%d</td><td class="num">%d</td>%s%s<td>%s</td></tr>`,
				rowClass, idx+1, mcHTMLEsc(r.Database), mcHTMLEsc(badgeClass), mcHTMLEsc(compareDBStatusCN(r.Status)),
				r.TotalTables, r.PassTables, r.FailTables, r.WarnTables, r.SkipTables, diffHTML, durHTML, errStr)
		}
		sb.WriteString(`</tbody></table></div>`)
	}

	sb.WriteString(`<div class="footer">由 TiMS (TiDB Migration Suite) 生成 &middot; 数据比对（多库汇总） &middot; <span class="tims-version">V`)
	sb.WriteString(mcHTMLEsc(dcReportVersion))
	sb.WriteString(`</span> &middot; `)
	sb.WriteString(mcHTMLEsc(time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString(`</div></div></body></html>`)
	return sb.String()
}
