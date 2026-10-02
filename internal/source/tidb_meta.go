package source

// Ruling seq 82 ①: register the "tidb" KIND so NormalizeKind passes it and
// /sources can describe it. TiDB is TARGET-only today (three 400 guards reject
// it as a source), so every capability bit is false — that keeps UI greying
// and the API guards on the same truth (five-pin #2). The single flip point
// for a future tidb→tidb same-protocol source lives here: flip the bits AND
// remove the tidb-as-source guards, never one without the other, and only
// after dedicated testing.
func init() {
	RegisterMeta("tidb", SourceMeta{
		DisplayName:  "TiDB",
		Implemented:  false,
		DefaultPort:  4000,
		NotImplMsg:   "TiDB 当前为目标端专用，作源端启用须专项实测后翻位",
		Capabilities: Capabilities{}, // all false: target-only status quo
		Fields: []FieldSpec{
			{Key: "host", Label: "主机地址", Type: "text", Required: true, Default: "localhost", Placeholder: "localhost", Group: "common"},
			{Key: "port", Label: "端口", Type: "number", Required: true, Default: 4000, Group: "common"},
			{Key: "user", Label: "用户名", Type: "text", Required: true, Default: "root", Group: "common"},
			{Key: "password", Label: "密码", Type: "password", Group: "common"},
			{Key: "database", Label: "数据库名", Type: "text", Required: true, Group: "common"},
		},
	})
}
