package source

// Register stub sources (oracle/mssql/db2) so the Web UI's source selector
// shows them as "即将支持" (disabled). They Open() with a friendly
// "not implemented" error (#t67 WSC).
func init() {
	Register("oracle", StubFactory("oracle"))
	Register("mssql", StubFactory("mssql"))
	Register("db2", StubFactory("db2"))
}
