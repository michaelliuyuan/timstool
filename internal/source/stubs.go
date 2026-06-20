package source

// Register stub sources (oracle/mssql/db2): their Open() returns a friendly
// "not implemented" error, AND they register a full SourceMeta (Implemented=
// false) so the Web UI can describe their future form fields in a disabled
// state without opening a connection (doc multi-source-web-form-design §5).
// The fields are forward-looking placeholders — adjusted when each adapter is
// actually implemented.
func init() {
	Register("oracle", StubFactory("oracle"))
	Register("mssql", StubFactory("mssql"))
	Register("db2", StubFactory("db2"))

	stubMsg := "该源暂未实现，敬请期待"

	RegisterMeta("oracle", SourceMeta{
		DisplayName:  "Oracle",
		Implemented:  false,
		DefaultPort:  1521,
		NotImplMsg:   stubMsg,
		Capabilities: Capabilities{},
		Fields: []FieldSpec{
			{Key: "host", Label: "主机地址", Type: "text", Required: true, Group: "common"},
			{Key: "port", Label: "端口", Type: "number", Required: true, Default: 1521, Group: "common"},
			{Key: "user", Label: "用户名", Type: "text", Required: true, Group: "common"},
			{Key: "password", Label: "密码", Type: "password", Group: "common"},
			{Key: "serviceName", Label: "Service Name", Type: "text", Group: "source"},
			{Key: "sid", Label: "SID", Type: "text", Group: "source"},
		},
	})
	RegisterMeta("mssql", SourceMeta{
		DisplayName:  "SQL Server",
		Implemented:  false,
		DefaultPort:  1433,
		NotImplMsg:   stubMsg,
		Capabilities: Capabilities{},
		Fields: []FieldSpec{
			{Key: "host", Label: "主机地址", Type: "text", Required: true, Group: "common"},
			{Key: "port", Label: "端口", Type: "number", Required: true, Default: 1433, Group: "common"},
			{Key: "user", Label: "用户名", Type: "text", Required: true, Group: "common"},
			{Key: "password", Label: "密码", Type: "password", Group: "common"},
			{Key: "database", Label: "数据库名", Type: "text", Required: true, Group: "common"},
			{Key: "instance", Label: "Instance", Type: "text", Group: "source"},
		},
	})
	RegisterMeta("db2", SourceMeta{
		DisplayName:  "DB2",
		Implemented:  false,
		DefaultPort:  50000,
		NotImplMsg:   stubMsg,
		Capabilities: Capabilities{},
		Fields: []FieldSpec{
			{Key: "host", Label: "主机地址", Type: "text", Required: true, Group: "common"},
			{Key: "port", Label: "端口", Type: "number", Required: true, Default: 50000, Group: "common"},
			{Key: "user", Label: "用户名", Type: "text", Required: true, Group: "common"},
			{Key: "password", Label: "密码", Type: "password", Group: "common"},
			{Key: "database", Label: "数据库名", Type: "text", Required: true, Group: "common"},
		},
	})
}
