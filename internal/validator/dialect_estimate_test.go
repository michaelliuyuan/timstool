package validator

// A7 estimate-recovery anchors (BUG-CMP-COUNT-P1, red-team checklist
// (a)-(f), leader order seq=60): the SHOW TABLE STATUS estimate is parsed
// by COLUMN NAME via scanShowTableStatusRows, immune to real-dialect
// column-count drift (MySQL 8.x / TiDB = 18 columns; the legacy fixed-dest
// Scan errored on every real estimate since #t95). Fixture knobs live on
// chunkScript (showCols17 / tgtEstNull / noRowsCol / showEmpty / tgtEstErr).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/common/reporter"
)

// --- (a) real 18-column shape, both dialects ---

// A16: mysqlDialect parses the real 18-column SHOW TABLE STATUS result and
// keeps the exact legacy query text (A7: query text is unchanged).
func TestEstimateRowsRealShapeMySQL(t *testing.T) {
	s := &chunkScript{tgtEst: "6000000"}
	db := s.open()
	defer db.Close()

	n, err := mysqlDialect{}.EstimateRows(context.Background(), db, "public", "t1")
	if err != nil || n != 6_000_000 {
		t.Fatalf("estimate = (%d, %v), want (6000000, nil)", n, err)
	}
	if got := s.sawQueries("SHOW TABLE STATUS"); len(got) != 1 || got[0] != "SHOW TABLE STATUS FROM `public` LIKE 't1'" {
		t.Fatalf("query = %v, want exact legacy text", got)
	}
}

// A17: tidbDialect parses the same 18-column shape with its bare-LIKE form.
func TestEstimateRowsRealShapeTiDB(t *testing.T) {
	s := &chunkScript{tgtEst: "8388608"}
	db := s.open()
	defer db.Close()

	n, err := tidbDialect{}.EstimateRows(context.Background(), db, "", "t1")
	if err != nil || n != 8_388_608 {
		t.Fatalf("estimate = (%d, %v), want (8388608, nil)", n, err)
	}
	if got := s.sawQueries("SHOW TABLE STATUS"); len(got) != 1 || got[0] != "SHOW TABLE STATUS LIKE 't1'" {
		t.Fatalf("query = %v, want exact legacy text", got)
	}
}

// --- (b) column-count drift still parses via name addressing ---

// A18: the 17-column 5.7-era shape (no Data_free) still yields the Rows
// estimate on both dialects - EqualFold name lookup, not position.
func TestEstimateRowsColumnDrift17(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    interface {
			EstimateRows(ctx context.Context, q dialectQueryer, schema, table string) (int64, error)
		}
		schema string
	}{
		{"mysql", mysqlDialect{}, "public"},
		{"tidb", tidbDialect{}, ""},
	} {
		s := &chunkScript{tgtEst: "424242", showCols17: true}
		db := s.open()
		n, err := tc.d.EstimateRows(context.Background(), db, tc.schema, "t1")
		db.Close()
		if err != nil || n != 424_242 {
			t.Fatalf("%s 17-col estimate = (%d, %v), want (424242, nil)", tc.name, n, err)
		}
	}
}

// --- (c) preserved QueryRow-era semantics ---

// A19: three-state semantics on both dialects - missing Rows column /
// NULL Rows / unparseable Rows yield (0, nil); zero result rows yield
// sql.ErrNoRows (the legacy missing-table signal quick.go falls back on);
// a query error propagates.
func TestEstimateRowsSemanticsPreserved(t *testing.T) {
	dialects := []struct {
		name string
		d    interface {
			EstimateRows(ctx context.Context, q dialectQueryer, schema, table string) (int64, error)
		}
		schema string
	}{
		{"mysql", mysqlDialect{}, "public"},
		{"tidb", tidbDialect{}, ""},
	}
	for _, dc := range dialects {
		cases := []struct {
			desc   string
			script *chunkScript
			errIs  error  // non-nil: require errors.Is(err, errIs)
			errHas string // non-empty: require err contains this text
		}{
			{"missing Rows column", &chunkScript{tgtEst: "9", noRowsCol: true}, nil, ""},
			{"NULL Rows", &chunkScript{tgtEstNull: true}, nil, ""},
			{"unparseable Rows", &chunkScript{tgtEst: "not-a-number"}, nil, ""},
			{"zero result rows", &chunkScript{tgtEst: "9", showEmpty: true}, sql.ErrNoRows, ""},
			{"query error", &chunkScript{tgtEstErr: fmt.Errorf("boom")}, nil, "boom"},
		}
		for _, c := range cases {
			db := c.script.open()
			n, err := dc.d.EstimateRows(context.Background(), db, dc.schema, "t1")
			db.Close()
			switch {
			case c.errIs != nil:
				if !errors.Is(err, c.errIs) {
					t.Fatalf("%s %s: err = %v, want %v", dc.name, c.desc, err, c.errIs)
				}
			case c.errHas != "":
				if err == nil || !strings.Contains(err.Error(), c.errHas) {
					t.Fatalf("%s %s: err = %v, want boom", dc.name, c.desc, err)
				}
			default:
				if err != nil || n != 0 {
					t.Fatalf("%s %s: estimate = (%d, %v), want (0, nil)", dc.name, c.desc, n, err)
				}
			}
		}
	}
}

// --- (d) routing anchors rerun with a MySQL-dialect SOURCE ---

// A20: mysql source, estimates below the threshold -> legacy whole-table
// path byte-identical (one COUNT per side, exact mysql-qualified text, no
// probes, no Suggestion). This is the small-table red line on the dialect
// whose zero anchor coverage hid the #t95 defect.
func TestChunkedCountRoutingSmallTableMySQLSource(t *testing.T) {
	src, dst, v := chunkHarness(chunkedCfg())
	src.tgtEst = "100" // mysql source estimates via SHOW TABLE STATUS too
	dst.tgtEst = "100"
	v.srcDialect = mysqlDialect{}
	tr := runChunkedCount(t, v, src, dst)

	if tr.Status != reporter.StatusPass || tr.SourceRows != 7 || tr.TargetRows != 7 {
		t.Fatalf("small table must count whole: %+v", tr)
	}
	if len(src.sawQueries("ORDER BY")) != 0 {
		t.Fatal("no boundary probes may run below the threshold")
	}
	srcCounts, dstCounts := src.countQueries(), dst.countQueries()
	if len(srcCounts) != 1 || len(dstCounts) != 1 {
		t.Fatalf("COUNTs src=%d dst=%d, want exactly 1/1 (legacy)", len(srcCounts), len(dstCounts))
	}
	if want := "SELECT COUNT(*) FROM `public`.`t1`"; srcCounts[0] != want {
		t.Fatalf("source COUNT = %q, want exact %q", srcCounts[0], want)
	}
	if tr.Suggestion != "" {
		t.Fatalf("legacy path must not set a Suggestion, got %q", tr.Suggestion)
	}
}

// A21: mysql source, estimates >= threshold -> the chunked path engages
// (probes + chunk COUNTs on the mysql side, PASS with equal sums). The P1
// value proposition on the dialect pair the user actually runs.
func TestChunkedCountRoutingLargeTableMySQLSource(t *testing.T) {
	src, dst, v := chunkHarness(chunkedCfg())
	src.tgtEst = "6000000"
	v.srcDialect = mysqlDialect{}
	tr := runChunkedCount(t, v, src, dst)

	if tr.Status != reporter.StatusPass || tr.SourceRows != 7 || tr.TargetRows != 7 {
		t.Fatalf("chunked count must pass: %+v", tr)
	}
	probes := 0
	for _, q := range src.sawQueries("ORDER BY") {
		if strings.Contains(q, "LIMIT") {
			probes++
		}
	}
	if probes != 4 {
		t.Fatalf("boundary probes = %d, want 4", probes)
	}
	if len(src.countQueries()) != 5 {
		t.Fatalf("chunk COUNTs = %d, want 5 (4 ranges + tail)", len(src.countQueries()))
	}
	if !strings.Contains(tr.Suggestion, "chunked count: 5 chunks") {
		t.Fatalf("Suggestion %q must disclose the chunked path", tr.Suggestion)
	}
}

// --- (e) quick-mode estimate recovery (ripple anchor) ---

// A22: with the estimate path repaired, quick mode consumes the SHOW TABLE
// STATUS estimate directly (no silent CountExact fallback). Before A7 the
// same input errored in Scan and EVERY quick run fell back to exact counts
// - the user-visible behavior change disclosed in the completion report.
func TestQuickModeEstimateRecoveryMySQL(t *testing.T) {
	src := &chunkScript{tgtEst: "100"}
	dst := &chunkScript{tgtEst: "100"}
	v := newChunkValidator(config.CompareConfig{})
	v.srcDialect = mysqlDialect{}

	srcDB := src.open()
	defer srcDB.Close()
	dstDB := dst.open()
	defer dstDB.Close()
	conn, err := dstDB.Conn(context.Background())
	if err != nil {
		t.Fatalf("get target conn: %v", err)
	}
	defer conn.Close()

	tr := v.validateQuick(context.Background(), srcDB, conn, "t1")
	if tr.Status != reporter.StatusPass {
		t.Fatalf("status=%s error=%q", tr.Status, tr.Error)
	}
	if tr.SourceRows != 100 || tr.TargetRows != 100 || tr.DiffRows != 0 {
		t.Fatalf("rows src=%d dst=%d diff=%d, want estimate 100/100/0", tr.SourceRows, tr.TargetRows, tr.DiffRows)
	}
	if len(src.countQueries()) != 0 || len(dst.countQueries()) != 0 {
		t.Fatal("estimate succeeded: no exact COUNT fallback may run")
	}
	if tr.Suggestion != "quick mode: row count estimation (no full scan)" {
		t.Fatalf("Suggestion = %q", tr.Suggestion)
	}
}
