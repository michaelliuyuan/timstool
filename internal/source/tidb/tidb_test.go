package tidb

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// MS-12b anchors: the adapter MUST run the tidb_version() blob through
// version.ShortTiDB — a full 15+ line build block echoed verbatim in the
// connection-test toast was the reported defect (user 09:07). These pin the
// adapter-level contract (blob → compact) with the version package's
// own four-shape anchors covered separately in version_test.go.
func TestVersionCompact(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	const blob = `Release Version: v7.1.9
Edition: Enterprise
Git Commit Hash: 76817a2b560e2fac74141f8bf08ac55b2a87b0c6
Git Branch: HEAD
UTC Build Time: 2026-08-06 05:45:36
GoVersion: go1.25.10
Race Enabled: false
Profile: release`

	mock.ExpectQuery("SELECT tidb_version\\(\\)").WillReturnRows(
		sqlmock.NewRows([]string{"tidb_version()"}).AddRow(blob))

	s := &Source{db: db}
	got, err := s.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if want := "TiDB v7.1.9"; got != want {
		t.Fatalf("Version = %q, want %q (adapter must compact the blob)", got, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations not met: %v", err)
	}
}

func TestVersionCompactFallback(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("SELECT tidb_version\\(\\)").WillReturnRows(
		sqlmock.NewRows([]string{"tidb_version()"}).AddRow("\n  not a blob \nsecond line"))

	s := &Source{db: db}
	got, err := s.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if want := "not a blob"; got != want {
		t.Fatalf("Version fallback = %q, want %q", got, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations not met: %v", err)
	}
}

func TestVersionNotConnected(t *testing.T) {
	s := &Source{}
	if _, err := s.Version(context.Background()); err == nil {
		t.Fatal("Version on unconnected source must error")
	}
}
