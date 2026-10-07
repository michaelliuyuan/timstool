package webapi

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/cdc"
	"github.com/michaelliuyuan/timstool/internal/common/config"
	"gopkg.in/yaml.v3"
)

// MS-11 pen 4 anchors: the MySQL side of the 全量+增量衔接 — chain prep
// records the master file:pos, and the post-migration seed writes it into
// the checkpoint file (never rewinding an existing newer checkpoint).
// All DB access is mocked (cdcProbe fake carrying the MySQL methods).

// fakeMySQLChainProber embeds the PG fake (satisfies cdcDBProber) and adds
// the cdcMySQLProber methods; only MySQLMasterStatus is exercised here.
type fakeMySQLChainProber struct {
	fakeProber
	masterFile string
	masterPos  uint32
	masterErr  error
}

func (f *fakeMySQLChainProber) PingTarget(cfg *config.Config) error   { return nil }
func (f *fakeMySQLChainProber) PingMySQL(cfg *config.Config) (string, error) {
	return "8.0.36", nil
}
func (f *fakeMySQLChainProber) MySQLVar(cfg *config.Config, name string) (string, error) {
	return "ROW", nil
}
func (f *fakeMySQLChainProber) MySQLReplPrivileges(cfg *config.Config) (bool, bool, error) {
	return true, false, nil
}
func (f *fakeMySQLChainProber) MySQLNoPKTables(cfg *config.Config) ([]string, error) {
	return nil, nil
}
func (f *fakeMySQLChainProber) MySQLMasterStatus(cfg *config.Config) (string, uint32, int64, error) {
	return f.masterFile, f.masterPos, 0, f.masterErr
}

func newMySQLChainServer(t *testing.T) (*Server, *fakeMySQLChainProber, string) {
	t.Helper()
	s, _ := newTestServer(t)
	cfgFile := filepath.Join(t.TempDir(), "config.yaml")
	cfg := config.DefaultConfig()
	cfg.Source.Type = "mysql"
	cfg.Source.Host = "myhost"
	cfg.Source.Port = 3306
	cpFile := filepath.Join(t.TempDir(), "cp.json")
	cfg.CDC.CheckpointFile = cpFile
	raw, _ := yaml.Marshal(cfg)
	if err := os.WriteFile(cfgFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s.SetCDCConfigFile(cfgFile)
	p := &fakeMySQLChainProber{masterFile: "mysql-bin.000003", masterPos: 157}
	p.version = "16.2" // PG fake fields unused on the mysql path
	s.cdcProbe = p
	return s, p, cpFile
}

// Anchor 1: chain prep records the current master file:pos (canonical
// rendering) and reports nothing provisioned.
func TestPrepareCDCChainMySQL(t *testing.T) {
	s, p, _ := newMySQLChainServer(t)
	pos, reused, err := s.prepareCDCChainFor(&config.Config{Source: config.SourceConfig{Type: "mysql"}})
	if err != nil {
		t.Fatalf("prepareCDCChainFor(mysql): %v", err)
	}
	if pos != "mysql-bin.000003:157" {
		t.Fatalf("chain start = %q, want mysql-bin.000003:157", pos)
	}
	if reused {
		t.Fatal("reused = true; nothing is provisioned on mysql")
	}
	// Probe failure is loud (better not to run than lose the window).
	p.masterErr = fmt.Errorf("log_bin off")
	if _, _, err := s.prepareCDCChainFor(&config.Config{Source: config.SourceConfig{Type: "mysql"}}); err == nil {
		t.Fatal("master-status failure was swallowed")
	}
}

// Anchor 2: the dispatch — a PG config still routes to the PG provisioner
// (slot probe untouched by the mysql path).
func TestPrepareCDCChainDispatch(t *testing.T) {
	s, _, _ := newMySQLChainServer(t)
	s.cdcChainProbe = &fakeChainProber{slotLSN: "0/3D0000A0"}
	lsn, _, err := s.prepareCDCChainFor(&config.Config{})
	if err != nil || lsn != "0/3D0000A0" {
		t.Fatalf("prepareCDCChainFor(pg) = %q, %v; want the slot consistent point", lsn, err)
	}
}

// Anchor 3: the seed writes the recorded chain file:pos into the checkpoint
// file; an existing checkpoint at or past the chain point wins (no rewind).
func TestSeedChainCheckpointMySQL(t *testing.T) {
	s, _, cpFile := newMySQLChainServer(t)
	cfg := &config.Config{Source: config.SourceConfig{Type: "mysql"}}
	cfg.Migration.ChainStartLSN = "mysql-bin.000003:157"

	if err := s.seedChainCheckpoint("t1", cfg); err != nil {
		t.Fatalf("seed: %v", err)
	}
	mgr := cdc.NewCheckpointManager(cpFile)
	cp, err := mgr.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cp.Binlog == nil || cp.Binlog.File != "mysql-bin.000003" || cp.Binlog.Pos != 157 {
		t.Fatalf("seeded checkpoint binlog = %+v, want mysql-bin.000003:157", cp.Binlog)
	}

	// An existing checkpoint PAST the chain point is kept (never rewind).
	mgr.UpdateBinlog(cdc.BinlogPosition{File: "mysql-bin.000004", Pos: 4})
	if err := mgr.Save(); err != nil {
		t.Fatal(err)
	}
	if err := s.seedChainCheckpoint("t2", cfg); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	cp2, err := cdc.NewCheckpointManager(cpFile).Load()
	if err != nil {
		t.Fatal(err)
	}
	if cp2.Binlog.File != "mysql-bin.000004" || cp2.Binlog.Pos != 4 {
		t.Fatalf("checkpoint rewound: %+v; want mysql-bin.000004:4", cp2.Binlog)
	}

	// A malformed ChainStartLSN fails loudly, never silently mis-seeds.
	bad := &config.Config{Source: config.SourceConfig{Type: "mysql"}}
	bad.Migration.ChainStartLSN = "not-a-position"
	if err := s.seedChainCheckpoint("t3", bad); err == nil {
		t.Fatal("malformed ChainStartLSN was accepted")
	}
}

// Anchor 4: parseChainBinlogPos pin (canonical form, LastIndex so future
// host:port-like strings cannot confuse it).
func TestParseChainBinlogPos(t *testing.T) {
	bp, err := parseChainBinlogPos("mysql-bin.000003:157")
	if err != nil || bp.File != "mysql-bin.000003" || bp.Pos != 157 {
		t.Fatalf("parse = %+v, %v", bp, err)
	}
	for _, bad := range []string{"", ":157", "mysql-bin.000003:", "a:b", "no-colon"} {
		if _, err := parseChainBinlogPos(bad); err == nil {
			t.Fatalf("parseChainBinlogPos(%q) accepted", bad)
		}
	}
}
