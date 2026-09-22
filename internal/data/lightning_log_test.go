package data

import "testing"

// Lightning stdout filtering (逐表日志精简): exactly ONE visible line per
// table — the "restore file completed" line (one per exported CSV, carrying
// table=`db`.`tbl`); restore-table/engine lifecycle lines and counters stay
// filtered to keep the task log concise; checksum WARN surfaces; ERROR/FATAL
// escalate; procedure lifecycle lines pass.
func TestFilterLightningLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want lightningLogLevel
	}{
		{"restore file completed passes", `[INFO] [restore.go:196] ["restore file completed"] [table=` + "`db`.`t1`" + `] [file=db.t1.0.csv] [imported=67]`, logInfo},
		{"restore table start filtered", `[INFO] [restore.go:123] ["restore table ` + "`db`.`t1`" + `" start]`, logDrop},
		{"restore table completed filtered", `[INFO] [restore.go:123] ["restore table ` + "`db`.`t1`" + `" completed]`, logDrop},
		{"checksum for table filtered", `[INFO] ["checksum for table ` + "`db`.`t2`" + `"]`, logDrop},
		{"import engine filtered", `[INFO] [import.go:505] ["import engine".EngineID=1]`, logDrop},
		{"restore engine closed filtered", `[INFO] [restore.go:300] ["restore engine closed" EngineID=2]`, logDrop},
		{"whole procedure start", `[INFO] [import.go:505] ["the whole procedure start"]`, logInfo},
		{"whole procedure completed", `[INFO] [import.go:533] ["the whole procedure completed"] [takeTime=4.5s]`, logInfo},
		{"lightning exit", `[INFO] ["tidb lightning exit"]`, logInfo},
		{"warn filtered", `[WARN] [pd.go:100] ["some warning"]`, logDrop},
		{"checksum warn surfaced", `[WARN] [restore.go:12] ["checksum failed" table=` + "`db`.`t3`" + `]`, logWarn},
		{"error escalated", `[ERROR] [restore.go:9] ["boom"]`, logError},
		{"fatal escalated", `[FATAL] [restore.go:9] ["boom"]`, logError},
		{"cfg dump filtered", `[INFO] [cfg.go:300] {"cfg": {...,"do-tables":[...],"table-rules":[...]}}`, logDrop},
		{"total tables counter filtered", `[INFO] [restore.go:8] ["total tables" count=5]`, logDrop},
		{"unrelated info filtered", `[INFO] [version.go:1] ["welcome to TiDB Lightning"]`, logDrop},
		{"debug filtered", `[DEBUG] [table.go:1] ["table debug"]`, logDrop},
	}
	for _, c := range cases {
		if got := filterLightningLine(c.line); got != c.want {
			t.Errorf("%s: filterLightningLine(%q) = %v, want %v", c.name, c.line, got, c.want)
		}
	}
}
