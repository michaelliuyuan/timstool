package webapi

// MS-11d anchors: source-type switch clears the stale checkpoint at PUT
// time, and GET /cdc/checkpoint serves the DISK truth (source-neutral
// position render) instead of the status.json embedded copy.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/michaelliuyuan/timstool/internal/cdc"
)

func writeGuardCheckpoint(t *testing.T, path string, cp cdc.Checkpoint) {
	t.Helper()
	data, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCDCConfigSourceTypeSwitchDropsStaleCheckpoint(t *testing.T) {
	s, _, _ := newCDCServer(t)

	// A PG checkpoint from the previous life of this config.
	cpPath := s.cdcCfgCheckpointPath(t)
	writeGuardCheckpoint(t, cpPath, cdc.Checkpoint{
		LSN:       0x1A2B3C4,
		Timestamp: time.Now(),
	})

	// Same-kind PUT (host change only): checkpoint survives.
	w, req := doReq("PUT", "/api/v1/cdc/config", `{"source":{"host":"pghost2"}}`)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("same-kind put = %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(cpPath); err != nil {
		t.Fatal("same-type host change must NOT drop the checkpoint")
	}

	// Cross-source PUT (type switch): stale checkpoint discarded.
	w, req = doReq("PUT", "/api/v1/cdc/config", `{"source":{"type":"mysql","host":"myhost"}}`)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("type-switch put = %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(cpPath); !os.IsNotExist(err) {
		t.Fatal("source type switch must drop the old-source checkpoint file")
	}
}

func TestCDCCheckpointEndpointServesDiskTruth(t *testing.T) {
	s, _, _ := newCDCServer(t)
	s.cdcEnabled = true
	cpPath := s.cdcCfgCheckpointPath(t)

	// A MySQL binlog checkpoint on disk with NO running child status -?the
	// endpoint must surface the file, not the (absent) status.json copy.
	writeGuardCheckpoint(t, cpPath, cdc.Checkpoint{
		Binlog:    &cdc.BinlogPosition{File: "binlog.000005", Pos: 6636},
		Timestamp: time.Now(),
	})
	w, req := doReq("GET", "/api/v1/cdc/checkpoint", "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("checkpoint = %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"exists":true`) {
		t.Fatalf("disk checkpoint not surfaced: %s", body)
	}
	// Source-neutral render: binlog file:pos, never a bare "0/0" LSN.
	if !strings.Contains(body, `binlog.000005:6636`) || strings.Contains(body, `"0/0"`) {
		t.Fatalf("checkpoint position must render source-neutral: %s", body)
	}

	// File gone -> empty object, no phantom resume point.
	os.Remove(cpPath)
	w, req = doReq("GET", "/api/v1/cdc/checkpoint", "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"exists":true`) {
		t.Fatalf("missing checkpoint must read as absent: %d %s", w.Code, w.Body.String())
	}
}

// cdcCfgCheckpointPath resolves the checkpoint path exactly like the child
// does (cdc.checkpoint_file from the wired config.yaml, default relative).
func (s *Server) cdcCfgCheckpointPath(t *testing.T) string {
	t.Helper()
	cfg, err := s.loadCDCConfig()
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.CDC.CheckpointFile
	if p == "" {
		p = ".cdc_checkpoint.json"
	}
	if !filepath.IsAbs(p) {
		abs, aerr := filepath.Abs(p)
		if aerr != nil {
			t.Fatal(aerr)
		}
		p = abs
	}
	return p
}
