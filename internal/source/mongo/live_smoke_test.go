package mongo

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/michaelliuyuan/timstool/internal/source"
)

// smokeAddr parses TIMSTOOL_MONGO_SMOKE ("host", "host:port", or empty→default).
func smokeAddr(v string) (string, int) {
	host, port := "localhost", 27017
	if v == "" {
		return host, port
	}
	for i := len(v) - 1; i >= 0; i-- {
		if v[i] == ':' {
			if p, err := strconv.Atoi(v[i+1:]); err == nil && p > 0 {
				host, port = v[:i], p
			}
			break
		}
	}
	if host == "" {
		host = "localhost"
	}
	return host, port
}

// TestLiveSmoke is the environment-gated live-DB smoke (docker container
// timstool-mongo-smoke, mongo:7 on :27017; M2 kickoff item covering the M1
// debt: collStats-driven storageSize/totalIndexSize fill). Skipped unless
// TIMSTOOL_MONGO_SMOKE is set (host[:port] optional, default localhost:27017).
func TestLiveSmoke(t *testing.T) {
	if os.Getenv("TIMSTOOL_MONGO_SMOKE") == "" {
		t.Skip("TIMSTOOL_MONGO_SMOKE not set; live smoke requires a running mongod")
	}
	host, port := smokeAddr(os.Getenv("TIMSTOOL_MONGO_SMOKE"))

	src, err := source.Open("mongodb", source.SourceConfig{Host: host, Port: port})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer src.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := src.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Level 1: databases.
	browser, ok := src.(source.MetaBrowser)
	if !ok {
		t.Fatal("not a MetaBrowser")
	}
	dbs, err := browser.ListDatabases(ctx)
	if err != nil {
		t.Fatalf("ListDatabases: %v", err)
	}
	var sawSmoke bool
	for _, d := range dbs {
		if d.Name == "smoke" {
			sawSmoke = true
		}
	}
	if !sawSmoke {
		t.Fatalf("databases = %+v, want smoke present", dbs)
	}

	// Level 2: collections incl. the M1-debt collStats fields.
	colls, err := browser.ListCollections(ctx, "smoke")
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	byName := map[string]source.CollectionInfo{}
	for _, c := range colls {
		byName[c.Name] = c
	}

	users, ok := byName["users"]
	if !ok {
		t.Fatalf("collections = %+v, want users", colls)
	}
	if users.Count != 50 {
		t.Errorf("users.Count = %d, want 50 (exact via collStats)", users.Count)
	}
	if users.StorageSize <= 0 {
		t.Errorf("users.StorageSize = %d, want > 0 (M1 debt: collStats fill)", users.StorageSize)
	}
	if users.TotalIndexSize <= 0 {
		t.Errorf("users.TotalIndexSize = %d, want > 0", users.TotalIndexSize)
	}

	// Compound index {name:1, age:-1}: order-preserved keys.
	var compound bool
	for _, ix := range users.Indexes {
		if len(ix.Keys) == 2 && ix.Keys[0].Field == "name" && ix.Keys[1].Field == "age" {
			if v1, ok := ix.Keys[0].Order.(int32); ok && v1 == 1 {
				if v2, ok := ix.Keys[1].Order.(int32); ok && v2 == -1 {
					compound = true
				}
			}
		}
	}
	if !compound {
		t.Errorf("users.Indexes = %+v, want compound {name:1,age:-1} with order", users.Indexes)
	}

	// Unique flag on {name:1}.
	var uniqueName bool
	for _, ix := range users.Indexes {
		if len(ix.Keys) == 1 && ix.Keys[0].Field == "name" && ix.Unique {
			uniqueName = true
		}
	}
	if !uniqueName {
		t.Errorf("users.Indexes missing unique {name:1}: %+v", users.Indexes)
	}

	// TTL index passthrough (the M2 engine skips TTL collections via this).
	events, ok := byName["events"]
	if !ok {
		t.Fatalf("collections missing events: %+v", colls)
	}
	var ttl *int32
	for _, ix := range events.Indexes {
		if ix.ExpireAfterSeconds != nil {
			ttl = ix.ExpireAfterSeconds
		}
	}
	if ttl == nil || *ttl != 3600 {
		t.Errorf("events TTL index = %+v, want expireAfterSeconds=3600", events.Indexes)
	}

	// View: no stats, no indexes.
	view, ok := byName["usersView"]
	if !ok {
		t.Fatalf("collections missing usersView: %+v", colls)
	}
	if view.Type != "view" || view.Count != -1 || view.Indexes != nil {
		t.Errorf("usersView = %+v, want type=view, Count=-1, Indexes=nil", view)
	}
}
