// collect.go — catalog inventory collectors. The MySQL walk serves BOTH
// the MySQL source and the TiDB target (same protocol, same
// information_schema predicates as the ddlexport walks); the PG walk
// mirrors ddlexport's pg_catalog queries ($1 placeholders, pgx stdlib).
// Every class query degrades independently: a class the server cannot
// answer (e.g. a TiDB build without information_schema.ROUTINES, or a
// scan account without mysql.user SELECT privilege) becomes a warning +
// empty set, never a failed run.
//
// v1.3 对比面: 表类采集 主键+字符集+排序规则 (字段级与索引级已移除); 视图/
// 例程/触发器/序列 仍为清单类; 用户+权限 走 mysql.user 全局权限列白名单
// (V3 只读红线: 只显式 SELECT 白名单列, 永不触碰 authentication_string/
// 密码哈希; 无权限时降级 WARN+空集).
package ddlcompare

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/michaelliuyuan/timstool/internal/source/mysql"
)

// mysqlSystemSchema consults the shared exported system-database set from
// the MySQL source adapter (single source of truth; ddlexport reads the
// same copy).
func mysqlSystemSchema(name string) bool {
	return mysql.SystemDatabases[strings.ToLower(name)]
}

// ListMySQLSchemas lists non-system databases over the MySQL protocol
// (MySQL source or TiDB target), sorted for a deterministic UI.
func ListMySQLSchemas(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SHOW DATABASES`)
	if err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		if mysqlSystemSchema(s) {
			continue
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// CountMySQLTopLevel cheaply counts the top-level objects the MySQL walk
// would inventory (tables + views + routines + triggers; v1.3 adds no
// sequences — MySQL/TiDB have no SEQUENCE object type) for the cap
// PRECHECK — before the heavy collection. A server missing one of the
// list tables (e.g. a TiDB build without information_schema.ROUTINES)
// returns an error; the caller then skips the precheck and the exact
// post-collection check remains.
func CountMySQLTopLevel(ctx context.Context, db *sql.DB, database string) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM information_schema.TABLES
			 WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'BASE TABLE') +
			(SELECT COUNT(*) FROM information_schema.VIEWS
			 WHERE TABLE_SCHEMA = ?) +
			(SELECT COUNT(*) FROM information_schema.ROUTINES
			 WHERE ROUTINE_SCHEMA = ?) +
			(SELECT COUNT(*) FROM information_schema.TRIGGERS
			 WHERE TRIGGER_SCHEMA = ?)`,
		database, database, database, database).Scan(&n)
	return n, err
}

// CollectMySQL inventories one database over the MySQL protocol (MySQL
// source or TiDB target). Structural classes (tables) are load-bearing and
// fail the run on error; list classes (views, routines, triggers,
// sequences, users, privileges) degrade to a warning + empty set.
func CollectMySQL(ctx context.Context, db *sql.DB, database string) (*Snapshot, []string, error) {
	snap := &Snapshot{Dialect: "mysql", Schema: database}
	var warns []string

	tables, err := mysqlTableMeta(ctx, db, database)
	if err != nil {
		return nil, nil, fmt.Errorf("read tables: %w", err)
	}
	snap.Tables = tables

	snap.Views = mysqlNames(ctx, db, `
		SELECT TABLE_NAME FROM information_schema.VIEWS
		WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME`, database, "视图", &warns)
	snap.Procedures, snap.Functions = mysqlRoutines(ctx, db, database, &warns)
	// Trigger keys are table-qualified ("table.trigger") — the D1 twin of
	// the PG walk: names alone cannot tell same-named triggers on
	// different tables apart, and the diff engine compares count + set.
	snap.Triggers = mysqlNames(ctx, db, `
		SELECT CONCAT(EVENT_OBJECT_TABLE, '.', TRIGGER_NAME)
		FROM information_schema.TRIGGERS
		WHERE TRIGGER_SCHEMA = ? ORDER BY EVENT_OBJECT_TABLE, TRIGGER_NAME`, database, "触发器", &warns)
	// TiDB (v7.1.9) implements CREATE SEQUENCE and lists sequence objects
	// in information_schema.TABLES as TABLE_TYPE='SEQUENCE' (attributes
	// live in information_schema.SEQUENCES); vanilla MySQL has none, the
	// query yields an empty set there and the class compares as absent.
	snap.Sequences = mysqlNames(ctx, db, `
		SELECT TABLE_NAME FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'SEQUENCE'
		ORDER BY TABLE_NAME`, database, "序列", &warns)
	snap.Users = mysqlUsers(ctx, db, &warns)
	return snap, warns, nil
}

// mysqlTableMeta reads every table's charset + collation + primary key in
// two queries (v1.3 表类口径). The charset is derived from the collation
// name prefix — MySQL/TiDB collation names are "<charset>_<suffix>" by
// convention (utf8mb4_general_ci → utf8mb4); an empty TABLE_COLLATION
// leaves the charset empty (both sides then compare equal-by-absence).
func mysqlTableMeta(ctx context.Context, db *sql.DB, database string) ([]TableMeta, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT TABLE_NAME, TABLE_COLLATION
		FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'BASE TABLE'
		ORDER BY TABLE_NAME`, database)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// F1: index-based addressing only — appending past the initial cap
	// reallocates the backing array, so element pointers captured earlier
	// would dangle and their PK writes land in the abandoned array
	// (>16 tables silently lost their PKs and false-failed).
	tables := make([]TableMeta, 0, 16)
	idx := map[string]int{}
	for rows.Next() {
		var name string
		var collation sql.NullString
		if err := rows.Scan(&name, &collation); err != nil {
			return nil, err
		}
		t := TableMeta{Name: name}
		if collation.Valid && collation.String != "" {
			t.Collation = collation.String
			if i := strings.IndexByte(collation.String, '_'); i > 0 {
				t.Charset = collation.String[:i]
			}
		}
		tables = append(tables, t)
		idx[name] = len(tables) - 1
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	pkRows, err := db.QueryContext(ctx, `
		SELECT TABLE_NAME, COLUMN_NAME
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = ? AND COLUMN_KEY = 'PRI'
		ORDER BY TABLE_NAME, ORDINAL_POSITION`, database)
	if err != nil {
		return nil, err
	}
	defer pkRows.Close()
	for pkRows.Next() {
		var table, column string
		if err := pkRows.Scan(&table, &column); err != nil {
			return nil, err
		}
		if i, ok := idx[table]; ok {
			tables[i].PK = append(tables[i].PK, column)
		}
	}
	return tables, pkRows.Err()
}

// mysqlUserPrivColumns is the mysql.user GLOBAL-privilege column whitelist
// (V3 只读红线 ①): the collector selects ONLY these explicit columns —
// never SELECT *, never authentication_string / password_lifetime, so no
// password hash can ever enter task.json / result.json / the report.
var mysqlUserPrivColumns = []string{
	"Select_priv", "Insert_priv", "Update_priv", "Delete_priv",
	"Create_priv", "Drop_priv", "Reload_priv", "Shutdown_priv",
	"Process_priv", "File_priv", "Grant_priv", "References_priv",
	"Index_priv", "Alter_priv", "Show_db_priv", "Super_priv",
	"Create_tmp_table_priv", "Lock_tables_priv", "Execute_priv",
	"Repl_slave_priv", "Repl_client_priv", "Create_view_priv",
	"Show_view_priv", "Create_routine_priv", "Alter_routine_priv",
	"Create_user_priv", "Event_priv", "Trigger_priv",
}

// mysqlSystemUsers is the set of built-in accounts filtered from the user
// inventory (the PG twin filters pg_* / information_schema roles). root is
// a real account and is kept.
var mysqlSystemUsers = map[string]bool{
	"mysql.session":      true,
	"mysql.sys":          true,
	"mysql.infoschema":   true,
	"mysql.schema":       true,
	"information_schema": true,
}

// mysqlUsers collects user + global-privilege rows over the mysql.user
// privilege-column WHITELIST (V3). It first intersects the whitelist with
// the columns mysql.user actually has (version/TiDB variance), then SELECTs
// exactly {User, Host} + the intersection — an explicit, hash-free query.
// On any failure (no mysql-library privilege, missing table) it degrades to
// a warning + empty set, exactly like the ROUTINES policy; the run never
// fails over the users/privileges classes.
func mysqlUsers(ctx context.Context, db *sql.DB, warns *[]string) []UserMeta {
	cols, err := mysqlUserExistingPrivCols(ctx, db)
	if err != nil {
		*warns = append(*warns, "用户/权限清单不可用（读取 mysql.user 列清单失败）："+err.Error())
		return nil
	}
	if len(cols) == 0 {
		*warns = append(*warns, "用户/权限清单不可用（mysql.user 无可用全局权限列；扫描账号可能缺少 mysql 库 SELECT 权限）")
		return nil
	}
	query := "SELECT User, Host, " + strings.Join(cols, ", ") + " FROM mysql.user"
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		*warns = append(*warns, "用户/权限清单不可用（mysql.user 查询失败；扫描账号可能缺少 mysql 库 SELECT 权限）："+err.Error())
		return nil
	}
	defer rows.Close()
	dest := make([]interface{}, 0, 2+len(cols))
	var out []UserMeta
	for rows.Next() {
		dest = dest[:0]
		var user, host string
		dest = append(dest, &user, &host)
		vals := make([]string, len(cols))
		for i := range vals {
			dest = append(dest, &vals[i])
		}
		if err := rows.Scan(dest...); err != nil {
			*warns = append(*warns, "用户/权限清单读取失败："+err.Error())
			return out
		}
		if mysqlSystemUsers[user] {
			continue
		}
		var privs []string
		for i, v := range vals {
			if strings.EqualFold(strings.TrimSpace(v), "Y") {
				privs = append(privs, cols[i])
			}
		}
		name := user
		if host != "" {
			name = user + "@" + host
		}
		out = append(out, UserMeta{Name: name, Privileges: privs})
	}
	if err := rows.Err(); err != nil {
		*warns = append(*warns, "用户/权限清单读取失败："+err.Error())
	}
	return out
}

// mysqlUserExistingPrivCols intersects the whitelist with the columns
// mysql.user actually has (information_schema.COLUMNS). A scan account
// without mysql-library visibility sees an empty intersection (or the
// query errors) — either way the caller degrades.
func mysqlUserExistingPrivCols(ctx context.Context, db *sql.DB) ([]string, error) {
	placeholders := make([]string, len(mysqlUserPrivColumns))
	args := make([]interface{}, len(mysqlUserPrivColumns))
	for i, c := range mysqlUserPrivColumns {
		placeholders[i] = "?"
		args[i] = c
	}
	query := `SELECT COLUMN_NAME FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = 'mysql' AND TABLE_NAME = 'user'
		AND COLUMN_NAME IN (` + strings.Join(placeholders, ", ") + `) ORDER BY COLUMN_NAME`
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// mysqlRoutines splits information_schema.ROUTINES into procedures and
// functions. A server without the table degrades to one warning and two
// empty sets — procedures/functions then compare as absent, visibly.
func mysqlRoutines(ctx context.Context, db *sql.DB, database string, warns *[]string) (procs, funcs []string) {
	rows, err := db.QueryContext(ctx, `
		SELECT ROUTINE_NAME, ROUTINE_TYPE FROM information_schema.ROUTINES
		WHERE ROUTINE_SCHEMA = ? ORDER BY ROUTINE_NAME`, database)
	if err != nil {
		*warns = append(*warns, "存储过程/自定义函数清单不可用（information_schema.ROUTINES 查询失败）："+err.Error())
		return nil, nil
	}
	defer rows.Close()
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			*warns = append(*warns, "存储过程/自定义函数清单读取失败："+err.Error())
			return procs, funcs
		}
		switch typ {
		case "PROCEDURE":
			procs = append(procs, name)
		case "FUNCTION":
			funcs = append(funcs, name)
		}
	}
	if err := rows.Err(); err != nil {
		*warns = append(*warns, "存储过程/自定义函数清单读取失败："+err.Error())
	}
	return procs, funcs
}

// mysqlNames runs a single-column name listing; on error it records a
// warning and returns nil so the class compares as an empty set.
func mysqlNames(ctx context.Context, db *sql.DB, query, schema, label string, warns *[]string) []string {
	rows, err := db.QueryContext(ctx, query, schema)
	if err != nil {
		*warns = append(*warns, label+"清单不可用："+err.Error())
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n sql.NullString
		if err := rows.Scan(&n); err != nil {
			*warns = append(*warns, label+"清单读取失败："+err.Error())
			return out
		}
		if n.Valid && n.String != "" {
			out = append(out, n.String)
		}
	}
	if err := rows.Err(); err != nil {
		*warns = append(*warns, label+"清单读取失败："+err.Error())
	}
	return out
}
