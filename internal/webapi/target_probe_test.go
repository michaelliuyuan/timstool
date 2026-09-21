package webapi

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func net_SplitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		t.Fatalf("port %q: %v", p, err)
	}
	return h, port
}

// TestProbePD covers the PD liveness probe used by the target connection
// test: a reachable PD returning cluster_id passes (and surfaces the id),
// an unreachable address fails with a descriptive error.
func TestProbePD(t *testing.T) {
	// Fake PD: /pd/api/v1/cluster returns {"cluster_id": 7252257174486103746}.
	pd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pd/api/v1/cluster" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]uint64{"cluster_id": 7252257174486103746})
	}))
	defer pd.Close()

	cid, err := probePD(context.Background(), pd.Listener.Addr().String())
	if err != nil {
		t.Fatalf("probePD reachable: %v", err)
	}
	if cid != "7252257174486103746" {
		t.Errorf("cluster id = %q, want 7252257174486103746", cid)
	}

	// Closed port 鈫?connection refused 鈫?descriptive error (no panic).
	closed := httptest.NewServer(http.NotFoundHandler())
	addr := closed.Listener.Addr().String()
	closed.Close()
	if _, err := probePD(context.Background(), addr); err == nil {
		t.Error("probePD on closed port should fail")
	}

	// Non-2xx/3xx 鈫?fail.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	if _, err := probePD(context.Background(), bad.Listener.Addr().String()); err == nil {
		t.Error("probePD on HTTP 500 should fail")
	}
}

// TestProbeTiDBStatus covers the TiDB status port probe: 2xx passes,
// unreachable or non-2xx fails with a descriptive error.
func TestProbeTiDBStatus(t *testing.T) {
	st := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer st.Close()

	host, port := net_SplitHostPort(t, st.Listener.Addr().String())
	if err := probeTiDBStatus(context.Background(), host, port); err != nil {
		t.Fatalf("probeTiDBStatus reachable: %v", err)
	}

	// Wrong port (closed) 鈫?fail.
	closed := httptest.NewServer(http.NotFoundHandler())
	cAddr := closed.Listener.Addr().String()
	closed.Close()
	chost, cport := net_SplitHostPort(t, cAddr)
	if err := probeTiDBStatus(context.Background(), chost, cport); err == nil {
		t.Error("probeTiDBStatus on closed port should fail")
	}

	// Non-2xx 鈫?fail.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer bad.Close()
	bhost, bport := net_SplitHostPort(t, bad.Listener.Addr().String())
	if err := probeTiDBStatus(context.Background(), bhost, bport); err == nil {
		t.Error("probeTiDBStatus on HTTP 404 should fail")
	}
}
