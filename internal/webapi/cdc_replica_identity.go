package webapi

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"

	"github.com/michaelliuyuan/timstool/internal/common/config"
)

// No-PK table assist (P1 task 2): the precheck's no_pk_tables warn gets a
// one-click remediation — generate / copy / execute
// `ALTER TABLE sch.tbl REPLICA IDENTITY FULL;` so UPDATE/DELETE can sync.
// Execution requires table ownership (or superuser); without the privilege
// the endpoint still returns the exact SQL for manual execution.

// replicaIdentityExecutor abstracts the ALTER execution (tests mock it).
// It returns the table's current owner-suitability: canAlter is false when
// the connecting user neither owns the table nor is superuser.
type replicaIdentityExecutor interface {
	CanAlter(cfg *config.Config, schema, table string) (bool, error)
	AlterFull(cfg *config.Config, schema, table string) error
}

// realReplicaIdentityExecutor runs the DDL on the source PG.
type realReplicaIdentityExecutor struct{}

func (realReplicaIdentityExecutor) CanAlter(cfg *config.Config, schema, table string) (bool, error) {
	db, err := pgDB(cfg)
	if err != nil {
		return false, err
	}
	defer db.Close()
	var can bool
	err = db.QueryRow(`
		SELECT pg_get_userbyid(c.relowner) = current_user
		       OR (SELECT rolsuper FROM pg_roles WHERE rolname = current_user)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2`, schema, table).Scan(&can)
	if err == sql.ErrNoRows {
		return false, fmt.Errorf("表 %s.%s 不存在", schema, table)
	}
	return can, err
}

func (realReplicaIdentityExecutor) AlterFull(cfg *config.Config, schema, table string) error {
	db, err := pgDB(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(fmt.Sprintf(`ALTER TABLE %s.%s REPLICA IDENTITY FULL`, schema, table))
	return err
}

// identRe restricts schema/table to plain identifiers — anything else (quoted,
// weird names) is refused and the operator gets the SQL to run manually.
var identRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_$]*$`)

type replicaIdentityResult struct {
	Table    string `json:"table"`
	SQL      string `json:"sql"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	CanAlter bool   `json:"can_alter"`
}

type replicaIdentityRequest struct {
	Confirm string   `json:"confirm"`
	Tables  []string `json:"tables"` // "schema.table" entries
}

// handleCDCReplicaIdentity executes ALTER TABLE ... REPLICA IDENTITY FULL for
// the given tables (POST /cdc/replica-identity, confirm:"ALTER"). Per-table
// results are reported individually; a permission failure still returns the
// SQL so the UI can fall back to copy-for-manual-execution.
func (s *Server) handleCDCReplicaIdentity(w http.ResponseWriter, r *http.Request) {
	var req replicaIdentityRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Confirm != "ALTER" {
		s.writeError(w, http.StatusBadRequest, `需要 {"confirm":"ALTER"} 确认`)
		return
	}
	if len(req.Tables) == 0 {
		s.writeError(w, http.StatusBadRequest, "tables 不能为空")
		return
	}
	cfg, err := func() (*config.Config, error) {
		cdcCfgMu.Lock()
		defer cdcCfgMu.Unlock()
		return s.loadCDCConfig()
	}()
	if err != nil {
		s.writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	exec := s.replicaIdentityExec
	if exec == nil {
		exec = realReplicaIdentityExecutor{}
	}

	results := make([]replicaIdentityResult, 0, len(req.Tables))
	defSchema := cfg.Source.Schema
	if defSchema == "" {
		defSchema = "public"
	}
	allOK := true
	for _, t := range req.Tables {
		schema, table, ok := splitTableRef(t, defSchema)
		stmt := fmt.Sprintf("ALTER TABLE %s.%s REPLICA IDENTITY FULL;", schema, table)
		if !ok {
			allOK = false
			results = append(results, replicaIdentityResult{Table: t, SQL: stmt, CanAlter: false,
				Error: "非法表名（仅支持普通标识符），请手动执行 SQL"})
			continue
		}
		res := replicaIdentityResult{Table: t, SQL: stmt}
		can, err := exec.CanAlter(cfg, schema, table)
		res.CanAlter = can
		switch {
		case err != nil:
			res.Error = err.Error()
		case !can:
			res.Error = "当前用户非表 owner 也非 superuser，无权执行；请复制 SQL 手动执行"
		default:
			if err := exec.AlterFull(cfg, schema, table); err != nil {
				res.Error = err.Error()
			} else {
				res.OK = true
			}
		}
		if !res.OK {
			allOK = false
		}
		results = append(results, res)
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":      allOK,
		"results": results,
		"hint":    "执行成功后请重新运行预检，表将从无主键预警清单移除",
	})
}

// splitTableRef parses "schema.table" (schema defaults to defSchema). ok=false
// when either part is not a plain identifier.
func splitTableRef(ref, defSchema string) (schema, table string, ok bool) {
	schema, table = "", ref
	for i := 0; i < len(ref); i++ {
		if ref[i] == '.' {
			schema, table = ref[:i], ref[i+1:]
			break
		}
	}
	if schema == "" {
		schema = defSchema
	}
	return schema, table, identRe.MatchString(schema) && identRe.MatchString(table)
}
