// collect_pg.go — the PostgreSQL side of the inventory (pg_catalog walk,
// $1 placeholders via pgx stdlib — same shape as ddlexport's PG queries).
// The same degrade policy applies: tables are load-bearing;
// views/routines/triggers/sequences/users degrade to warnings.
//
// v1.3 对比面: 表类 charset/collation 取数据库级 encoding/lc_collate 充当
// (PG 表级无独立值, v1.3 口径); 序列清单来自 pg_sequences; 用户+权限来自
// pg_roles 布尔 + pg_auth_members 成员名 (V3: pg_roles 无密码字段天然安全,
// pg_auth_members 只取成员名).
package ddlcompare

import (
	"context"
	"database/sql"
	"fmt"
)

// CountPGTopLevel is the PG twin of CountMySQLTopLevel: a cheap top-level
// object COUNT (tables + views + routines + triggers + sequences, the
// collector's own predicates) for the cap precheck. prokind (PG 11+)
// failing on an older server errors the whole query — the caller skips
// the precheck.
func CountPGTopLevel(ctx context.Context, db *sql.DB, schema string) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM pg_class c
			 JOIN pg_namespace ns ON ns.oid = c.relnamespace
			 WHERE ns.nspname = $1 AND c.relkind IN ('r', 'p')) +
			(SELECT COUNT(*) FROM information_schema.views
			 WHERE table_schema = $2) +
			(SELECT COUNT(*) FROM pg_proc p
			 JOIN pg_namespace ns ON ns.oid = p.pronamespace
			 WHERE ns.nspname = $3 AND p.prokind IN ('p', 'f')) +
			(SELECT COUNT(*) FROM pg_trigger t
			 JOIN pg_class c ON c.oid = t.tgrelid
			 JOIN pg_namespace ns ON ns.oid = c.relnamespace
			 WHERE ns.nspname = $4 AND NOT t.tgisinternal) +
			(SELECT COUNT(*) FROM pg_sequences
			 WHERE schemaname = $5)`,
		schema, schema, schema, schema, schema).Scan(&n)
	return n, err
}

// CollectPG inventories one PG schema.
func CollectPG(ctx context.Context, db *sql.DB, schema string) (*Snapshot, []string, error) {
	snap := &Snapshot{Dialect: "postgres", Schema: schema}
	var warns []string

	// Table-level charset/collation stand-in: PG tables carry no per-table
	// value, so the database encoding + lc_collate stand in (v1.3 口径).
	enc, coll, err := pgDatabaseCharset(ctx, db)
	if err != nil {
		return nil, nil, fmt.Errorf("read database charset: %w", err)
	}

	tables, err := pgList(ctx, db, `
		SELECT c.relname FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind IN ('r', 'p')
		ORDER BY c.relname`, schema)
	if err != nil {
		return nil, nil, fmt.Errorf("read tables: %w", err)
	}
	pk, err := pgPrimaryKey(ctx, db, schema)
	if err != nil {
		return nil, nil, fmt.Errorf("read primary keys: %w", err)
	}
	for _, t := range tables {
		snap.Tables = append(snap.Tables, TableMeta{
			Name: t, Charset: enc, Collation: coll, PK: pk[t],
		})
	}

	snap.Views = pgNames(ctx, db, `
		SELECT table_name FROM information_schema.views
		WHERE table_schema = $1 ORDER BY table_name`, schema, "视图", &warns)
	snap.Procedures, snap.Functions = pgRoutines(ctx, db, schema, &warns)
	// Trigger keys are table-qualified ("table.trigger"): PG tgname is
	// unique per table only, so ten same-named "audit" triggers on ten
	// tables must compare as ten distinct objects (D1).
	snap.Triggers = pgNames(ctx, db, `
		SELECT c.relname || '.' || t.tgname FROM pg_trigger t
		JOIN pg_class c ON c.oid = t.tgrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND NOT tgisinternal
		ORDER BY c.relname, t.tgname`, schema, "触发器", &warns)
	// Sequences (v1.3 new class; pg_sequences PG 10+).
	snap.Sequences = pgNames(ctx, db, `
		SELECT sequence_name FROM information_schema.sequences
		WHERE sequence_schema = $1 ORDER BY sequence_name`, schema, "序列", &warns)
	// Users + privileges (v1.3 new class): pg_roles booleans + role
	// memberships from pg_auth_members (member names only, V3).
	snap.Users = pgUsers(ctx, db, &warns)
	return snap, warns, nil
}

// ListPGSchemas lists non-system PG schemas (pg_namespace minus
// information_schema and the pg_* system namespaces).
func ListPGSchemas(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT n.nspname FROM pg_namespace n
		WHERE n.nspname <> 'information_schema'
		  AND n.nspname NOT LIKE 'pg\_%'
		ORDER BY n.nspname`)
	if err != nil {
		return nil, fmt.Errorf("list schemas: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func pgList(ctx context.Context, db *sql.DB, query, schema string) ([]string, error) {
	rows, err := db.QueryContext(ctx, query, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// pgDatabaseCharset reads the database-level encoding + lc_collate that
// stand in for per-table charset/collation on PG (v1.3 口径). encoding
// "UTF8" lowercases to the MySQL "utf8" bucket in the diff engine.
func pgDatabaseCharset(ctx context.Context, db *sql.DB) (charset, collation string, err error) {
	var enc, coll string
	err = db.QueryRowContext(ctx, `
		SELECT pg_encoding_to_char(d.encoding), d.datcollate
		FROM pg_database d WHERE d.datname = current_database()`).Scan(&enc, &coll)
	if err != nil {
		return "", "", err
	}
	return enc, coll, nil
}

// pgPrimaryKey reads PK membership via the information_schema constraint
// views (portable across PG versions; ordinal_position preserves key
// column order). The join carries all three identity keys — schema,
// constraint name AND table name: constraint names are unique per table,
// not per schema, so a hand-named identical constraint on two tables
// would otherwise cross-join and interleave the PK columns.
func pgPrimaryKey(ctx context.Context, db *sql.DB, schema string) (map[string][]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT tc.table_name, kcu.column_name
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON kcu.constraint_name = tc.constraint_name
		 AND kcu.table_schema = tc.table_schema
		 AND kcu.table_name = tc.table_name
		WHERE tc.table_schema = $1 AND tc.constraint_type = 'PRIMARY KEY'
		ORDER BY tc.table_name, kcu.ordinal_position`, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pk := map[string][]string{}
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			return nil, err
		}
		pk[table] = append(pk[table], column)
	}
	return pk, rows.Err()
}

// pgUsers collects accounts + global-privilege fingerprints from pg_roles
// (rolsuper/rolcreatedb/rolcreaterole/rolreplication booleans) plus role
// memberships from pg_auth_members. System built-in roles (pg_*,
// information_schema) are filtered; pg_roles carries no password column so
// no hash can leak. Degrades to warning + empty on any failure.
func pgUsers(ctx context.Context, db *sql.DB, warns *[]string) []UserMeta {
	rows, err := db.QueryContext(ctx, `
		SELECT rolname, rolsuper, rolcreatedb, rolcreaterole, rolreplication
		FROM pg_roles
		WHERE rolname NOT LIKE 'pg\_%' AND rolname <> 'information_schema'
		ORDER BY rolname`)
	if err != nil {
		*warns = append(*warns, "用户/权限清单不可用（pg_roles 查询失败）："+err.Error())
		return nil
	}
	defer rows.Close()
	users := make([]UserMeta, 0, 16)
	byName := map[string]*UserMeta{}
	for rows.Next() {
		var name string
		var super, createdb, createrole, replication bool
		if err := rows.Scan(&name, &super, &createdb, &createrole, &replication); err != nil {
			*warns = append(*warns, "用户/权限清单读取失败："+err.Error())
			return users
		}
		var privs []string
		if super {
			privs = append(privs, "SUPER")
		}
		if createdb {
			privs = append(privs, "CREATEDB")
		}
		if createrole {
			privs = append(privs, "CREATEROLE")
		}
		if replication {
			privs = append(privs, "REPLICATION")
		}
		users = append(users, UserMeta{Name: name, Privileges: privs})
		byName[name] = &users[len(users)-1]
	}
	if err := rows.Err(); err != nil {
		*warns = append(*warns, "用户/权限清单读取失败："+err.Error())
	}
	// Role memberships: member names only (V3 ④) — append as
	// "MEMBER:<rolename>" tokens so a granted role is visible in the
	// fingerprint without recursing into pg_roles of the member role.
	mRows, err := db.QueryContext(ctx, `
		SELECT m.rolname AS member, r.rolname AS role
		FROM pg_auth_members am
		JOIN pg_roles m ON m.oid = am.member
		JOIN pg_roles r ON r.oid = am.roleid
		WHERE m.rolname NOT LIKE 'pg\_%' AND r.rolname NOT LIKE 'pg\_%'
		ORDER BY member, role`)
	if err != nil {
		*warns = append(*warns, "角色成员清单不可用（pg_auth_members 查询失败）："+err.Error())
		return users
	}
	defer mRows.Close()
	for mRows.Next() {
		var member, role string
		if err := mRows.Scan(&member, &role); err != nil {
			*warns = append(*warns, "角色成员清单读取失败："+err.Error())
			return users
		}
		if u, ok := byName[member]; ok {
			u.Privileges = append(u.Privileges, "MEMBER:"+role)
		}
	}
	if err := mRows.Err(); err != nil {
		*warns = append(*warns, "角色成员清单读取失败："+err.Error())
	}
	return users
}

// pgRoutines splits pg_proc by prokind ('p' procedure / 'f' function).
// prokind exists on PG 11+ (ddlexport BUG-0934 precedent); an older
// server degrades to one warning + two empty sets.
func pgRoutines(ctx context.Context, db *sql.DB, schema string, warns *[]string) (procs, funcs []string) {
	rows, err := db.QueryContext(ctx, `
		SELECT p.proname, p.prokind FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = $1 AND p.prokind IN ('p', 'f')
		ORDER BY p.proname, p.prokind`, schema)
	if err != nil {
		*warns = append(*warns, "存储过程/自定义函数清单不可用（pg_proc.prokind 查询失败，PG 11 以下不支持）："+err.Error())
		return nil, nil
	}
	defer rows.Close()
	for rows.Next() {
		var name, kind string
		if err := rows.Scan(&name, &kind); err != nil {
			*warns = append(*warns, "存储过程/自定义函数清单读取失败："+err.Error())
			return procs, funcs
		}
		switch kind {
		case "p":
			procs = append(procs, name)
		case "f":
			funcs = append(funcs, name)
		}
	}
	if err := rows.Err(); err != nil {
		*warns = append(*warns, "存储过程/自定义函数清单读取失败："+err.Error())
	}
	return procs, funcs
}

func pgNames(ctx context.Context, db *sql.DB, query, schema, label string, warns *[]string) []string {
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
