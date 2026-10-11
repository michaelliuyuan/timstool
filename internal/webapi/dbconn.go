package webapi

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/michaelliuyuan/timstool/internal/common/config"
)

// pingTimeout bounds connection tests (F-06 item 3): a dead host must fail
// fast instead of stalling the handler for the driver's default dial timeout.
// The DSNs already carry connect_timeout; this covers the ping round-trip.
func pingTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}

// sourceConnSpec is the MS-09 dispatch: the NORMALIZED source type picks the
// driver and the DSN. mysql assembles the go-sql-driver DSN via DSNByType so
// the MS-08d time_zone='+00:00' UTC session pin rides on the DSN; every other
// kind (including empty/legacy, which SourceType normalizes to postgres) keeps
// the pgx pair byte-identical to the old openPGTestConn(sc.DSN()) call shape.
func sourceConnSpec(sc config.SourceConfig) (driver, dsn string) {
	if sc.SourceType() == "mysql" {
		return "mysql", sc.DSNByType()
	}
	return "pgx", sc.DSN()
}

// openSourceTestConn opens a source connection test (open+ping) dispatched on
// the normalized source type. PG sources behave byte-identically to the legacy
// openPGTestConn path; the per-site capability guards (what a source type may
// DO) stay at the handlers - this layer only routes the connection.
func openSourceTestConn(sc config.SourceConfig) (*sql.DB, error) {
	driver, dsn := sourceConnSpec(sc)
	conn, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect failed: %w", err)
	}
	ctx, cancel := pingTimeout()
	defer cancel()
	if err := conn.PingContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ping failed: %w", err)
	}
	return conn, nil
}

func openPGTestConn(dsn string) (*sql.DB, error) {
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("connect failed: %w", err)
	}
	ctx, cancel := pingTimeout()
	defer cancel()
	if err := conn.PingContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ping failed: %w", err)
	}
	return conn, nil
}

func openMySQLTestConn(dsn string) (*sql.DB, error) {
	conn, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("connect failed: %w", err)
	}
	ctx, cancel := pingTimeout()
	defer cancel()
	if err := conn.PingContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ping failed: %w", err)
	}
	return conn, nil
}
