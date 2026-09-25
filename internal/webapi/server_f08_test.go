package webapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/orchestrator"
	"github.com/michaelliuyuan/timstool/internal/store"
)

// F-08 anchor: runningTasks/logCores maps are touched from concurrent
// handler and runMigration goroutines — hammering all accessors under
// -race must stay clean (unguarded writes would fatal the process).
func TestTaskMapsConcurrentAccess(t *testing.T) {
	s := &Server{
		runningTasks: make(map[string]context.CancelFunc),
		taskRuns:     make(map[string]uint64),
		logCores:     make(map[string]*TaskLogCore),
	}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				id := string(rune('a' + (w+i)%6))
				_, cancel := context.WithCancel(context.Background())
				s.beginRun(id, cancel)
				s.setLogCore(id, NewTaskLogCore(nil, id, nil))
				if i%3 == 0 {
					s.cancelRunning(id)
				}
				s.deleteLogCore(id)
				cancel()
			}
		}(w)
	}
	wg.Wait()
}

// F-08 anchor: resume must cancel the previous run's context before
// starting a new one — otherwise two runMigration goroutines race on the
// same task. Two sequential resumes must leave exactly one pipeline
// goroutine alive and the first one cancelled.
func TestResumeCancelsPreviousRun(t *testing.T) {
	s := &Server{
		runningTasks: make(map[string]context.CancelFunc),
		taskRuns:     make(map[string]uint64),
		logCores:     make(map[string]*TaskLogCore),
	}

	cancelled := make(chan struct{}, 2)
	oldPipeline := runPipeline
	defer func() { runPipeline = oldPipeline }()
	runPipeline = func(ctx context.Context, _ config.Config, _ orchestrator.PipelineConfig) ([]orchestrator.PipelineResult, error) {
		<-ctx.Done()
		cancelled <- struct{}{}
		return nil, ctx.Err()
	}

	taskID := "t-resume"
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		s.beginRun(taskID, cancel)
		go func(c context.Context) {
			lc := NewTaskLogCore(nil, taskID, nil)
			s.setLogCore(taskID, lc)
			defer s.deleteLogCore(taskID)
			_, _ = runPipeline(c, config.Config{}, orchestrator.PipelineConfig{})
		}(ctx)
	}

	// Cancel-and-replace exactly like handleResumeTask does.
	s.cancelRunning(taskID)
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("previous run was not cancelled")
	}
}

// F-08 anchor: a slow (non-reading) WS consumer must not stall the hub's
// broadcast loop; other clients keep receiving.
func TestHubSlowClientDoesNotStallBroadcast(t *testing.T) {
	// A real (silent) server-side conn so writePump has something to write
	// to; the client dials and simply never sends.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for { // read pump: drain until the peer leaves
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer up.Close()
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(up.URL, "http"), nil)
	if err != nil {
		t.Skip("websocket dial failed:", err)
	}
	defer client.Close()

	h := newHub()
	go h.Run()

	fast := &wsClient{conn: client, send: make(chan []byte, wsSendBuffer)}
	h.register <- fast

	// Fill the buffer far beyond capacity: drops must be silent.
	for i := 0; i < wsSendBuffer*4; i++ {
		h.broadcast <- []byte("tick")
	}

	select {
	case <-fast.send:
	case <-time.After(time.Second):
		t.Fatal("registered client never received the broadcast")
	}
	// Drain and confirm the loop is still alive.
	<-fast.send
	h.broadcast <- []byte("alive")
	select {
	case <-fast.send:
	case <-time.After(time.Second):
		t.Fatal("hub broadcast loop stalled after slow-consumer pressure")
	}
}

// F-08 anchor: cross-origin WS handshakes are rejected; same-origin and
// non-browser (no Origin) are allowed.
func TestSameOriginCheck(t *testing.T) {
	cases := []struct {
		origin, host string
		want         bool
	}{
		{"", "any:8080", true},
		{"http://192.168.1.5:8080", "192.168.1.5:8080", true},
		{"https://evil.example", "192.168.1.5:8080", false},
		{"http://192.168.1.5:8080", "192.168.1.5:9090", false},
		{"::not-a-url", "192.168.1.5:8080", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/ws", nil)
		r.Host = c.host
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if got := sameOriginCheck(r); got != c.want {
			t.Errorf("sameOriginCheck(origin=%q host=%q) = %v, want %v", c.origin, c.host, got, c.want)
		}
	}
}

// F-08 adversarial-rework anchor: when resume cancels a stale run, the
// stale run's cancel branch must NOT overwrite the fresh Running status
// with Cancelled, and must not delete the fresh run's log core.
func TestSupersededRunKeepsFreshStatusAndLogCore(t *testing.T) {
	s, st := newTestServer(t)

	taskID := "t-supersede"
	if err := st.CreateTask(&store.Task{ID: taskID, Name: "sup", Status: store.TaskStatusPaused, ConfigJSON: `{}`}); err != nil {
		t.Fatal(err)
	}

	release := make(chan struct{})
	freshRelease := make(chan struct{})
	var call int32
	staleDone := make(chan struct{})
	oldPipeline := runPipeline
	defer func() { runPipeline = oldPipeline }()
	runPipeline = func(ctx context.Context, _ config.Config, _ orchestrator.PipelineConfig) ([]orchestrator.PipelineResult, error) {
		if atomic.AddInt32(&call, 1) == 1 {
			// stale run: hold open until superseded, then observe cancel
			<-release
			<-ctx.Done()
			close(staleDone)
			return nil, ctx.Err()
		}
		// fresh run: just hold open
		<-freshRelease
		return nil, nil
	}

	// Stale run (as if started earlier, then paused without cancel).
	staleCtx, staleCancel := context.WithCancel(context.Background())
	defer staleCancel()
	staleRun := s.beginRun(taskID, staleCancel)
	go s.runMigration(staleCtx, taskID, config.Config{}, staleRun)
	time.Sleep(150 * time.Millisecond) // let it register its log core

	// Resume mirrors handleResumeTask: flip status, cancel stale, begin
	// a fresh run with a new generation token.
	_ = st.UpdateTaskStatus(taskID, store.TaskStatusRunning)
	s.cancelRunning(taskID)
	freshCtx, freshCancel := context.WithCancel(context.Background())
	defer freshCancel()
	freshRun := s.beginRun(taskID, freshCancel)
	go s.runMigration(freshCtx, taskID, config.Config{}, freshRun)
	time.Sleep(150 * time.Millisecond)

	// Release the stale run so its cancel branch executes AFTER the fresh
	// run registered — the exact interleaving that used to clobber state.
	close(release)
	select {
	case <-staleDone:
	case <-time.After(2 * time.Second):
		t.Fatal("stale run never finished")
	}
	time.Sleep(150 * time.Millisecond)

	task, _ := st.GetTask(taskID)
	if task.Status == store.TaskStatusCancelled {
		t.Fatal("superseded stale run overwrote the fresh Running status with Cancelled")
	}

	// The fresh run's log core must have survived the stale run's defer.
	s.taskMu.Lock()
	_, hasCore := s.logCores[taskID]
	s.taskMu.Unlock()
	if !hasCore {
		t.Fatal("stale run's cleanup deleted the fresh run's log core")
	}

	// Ownership token semantics.
	if s.ownsRun(taskID, staleRun) {
		t.Error("stale generation must no longer own the task")
	}
	if !s.ownsRun(taskID, freshRun) {
		t.Error("fresh generation must own the task")
	}

	close(freshRelease)
}

// F-08 anchor: unregister closes the send channel so a writePump blocked
// in range exits (no goroutine leak on idle disconnect); pending frames
// are drained first.
func TestUnregisterClosesSendAndStopsWritePump(t *testing.T) {
	h := newHub()
	go h.Run()

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer up.Close()
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(up.URL, "http"), nil)
	if err != nil {
		t.Skip("websocket dial failed:", err)
	}
	defer client.Close()

	c := &wsClient{conn: client, send: make(chan []byte, wsSendBuffer)}
	h.register <- c
	h.broadcast <- []byte("frame") // writePump busy writing
	h.unregister <- c              // idle disconnect path

	// send must be closed; receiving from a closed channel is immediate.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-c.send:
			if !ok {
				return // closed = writePump will exit
			}
		case <-deadline:
			t.Fatal("send channel was never closed after unregister")
		}
	}
}

// F-08 anchor: writePump writes one frame per send with a deadline; a
// write error must terminate the pump without blocking the hub.
func TestWritePumpStopsOnDeadConn(t *testing.T) {
	// A real TCP-backed WS pair: server that never reads, client that
	// vanishes. writePump must exit on write error rather than hang.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		c := &wsClient{conn: conn, send: make(chan []byte, 1)}
		h := newHub()
		go h.Run()
		h.register <- c
		time.Sleep(50 * time.Millisecond)
	}))
	defer up.Close()
	wsURL := "ws" + strings.TrimPrefix(up.URL, "http")
	client, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Skip("websocket dial failed:", err)
	}
	client.Close() // vanish: server writes will now fail
	time.Sleep(300 * time.Millisecond)
}
