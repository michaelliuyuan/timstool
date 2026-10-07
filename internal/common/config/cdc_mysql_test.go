package config

// MS-11 pen 3 anchors: the MySQL CDC v1 config rules — server_id is a
// hard requirement, sync_ddl is rejected up front (v1 is DML-only; the
// runtime hard-stops DDL anyway), and PG sources keep validating unchanged.

import "testing"

func TestCDCMySQLServerIDRequired(t *testing.T) {
	c := DefaultConfig()
	c.CDC.Enable = true
	c.Target = TargetConfig{Host: "t", Port: 4000, Database: "tidb"}
	c.Source = SourceConfig{Type: "mysql", Host: "h", Port: 3306, Database: "db"}
	c.CDC.SyncDDL = false
	if err := c.Validate(); err == nil || !contains(err.Error(), "server_id") {
		t.Fatalf("mysql CDC without server_id must fail loud, got %v", err)
	}
	c.CDC.ServerID = 4242
	if err := c.Validate(); err != nil {
		t.Fatalf("with server_id set the config must validate: %v", err)
	}
}

func TestCDCMySQLSyncDDLRejected(t *testing.T) {
	c := DefaultConfig()
	c.CDC.Enable = true
	c.Target = TargetConfig{Host: "t", Port: 4000, Database: "tidb"}
	c.Source = SourceConfig{Type: "mysql", Host: "h", Port: 3306, Database: "db"}
	c.CDC.ServerID = 4242
	c.CDC.SyncDDL = true
	if err := c.Validate(); err == nil || !contains(err.Error(), "sync_ddl") {
		t.Fatalf("mysql CDC with sync_ddl=true must be rejected up front, got %v", err)
	}
	c.CDC.SyncDDL = false
	if err := c.Validate(); err != nil {
		t.Fatalf("sync_ddl=false must validate: %v", err)
	}
}

func TestCDCPGUnaffectedByServerID(t *testing.T) {
	c := DefaultConfig()
	c.CDC.Enable = true
	c.Target = TargetConfig{Host: "t", Port: 4000, Database: "tidb"}
	c.Source = SourceConfig{Type: "postgres", Host: "h", Port: 5432, Database: "db"}
	c.CDC.ServerID = 0
	c.CDC.SyncDDL = true // PG DDL tracking stays supported
	if err := c.Validate(); err != nil {
		t.Fatalf("PG CDC must keep validating unchanged (no server_id/sync_ddl rules): %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
