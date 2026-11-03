package source

// MS-12 flip (ruling seq 82 ①'s reserved flip point, user-approved
// 2026-11-02): TiDB becomes usable as a DDL-export source. The flip keeps
// the paired discipline — the two tidb-as-source bare guards in
// webapi/source_handler.go are removed in the same change (the capability
// bit takes over), while every OTHER bit stays false: the six non-target
// flows (schema/data/cdc/compare/watermark/assess) keep rejecting tidb
// via their srcCapable reads, and the migration-task tidb-as-source guard
// in server.go resolveTaskRefs remains. The Open factory lives in
// internal/source/tidb (connection lifecycle + tidb_version echo only).
func init() {
	RegisterMeta("tidb", SourceMeta{
		DisplayName: "TiDB",
		Implemented: true,
		DefaultPort: 4000,
		// DDLExport only (MS-12): the SHOW CREATE walk speaks the MySQL
		// wire; sequences are a real TiDB object (v5.0+ floor, product
		// claim v6.5 LTS+). Every other bit stays false.
		Capabilities: Capabilities{DDLExport: true},
		Fields: []FieldSpec{
			{Key: "host", Label: "主机地址", Type: "text", Required: true, Default: "localhost", Placeholder: "localhost", Group: "common"},
			{Key: "port", Label: "端口", Type: "number", Required: true, Default: 4000, Group: "common"},
			{Key: "user", Label: "用户名", Type: "text", Required: true, Default: "root", Group: "common"},
			{Key: "password", Label: "密码", Type: "password", Group: "common"},
			{Key: "database", Label: "数据库名", Type: "text", Required: true, Group: "common"},
		},
	})
}
