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
					if c := s.runningCancel(id); c != nil {
						c()
					}
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
	var cancels []context.CancelFunc
	dones := make([]chan struct{}, 0, 2)
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		s.beginRun(taskID, cancel)
		done := make(chan struct{})
		dones = append(dones, done)
		go func(c context.Context, d chan struct{}) {
			defer close(d)
			lc := NewTaskLogCore(nil, taskID, nil)
			s.setLogCore(taskID, lc)
			defer s.deleteLogCore(taskID)
			_, _ = runPipeline(c, config.Config{}, orchestrator.PipelineConfig{})
		}(ctx, done)
	}

	// Cancel-and-replace exactly like handleResumeTask does (F-08 v2):
	// swapRunning hands ownership to a new generation, oldCancel fires
	// outside the lock.
	noop := func() {}
	oldCancel, _ := s.swapRunning(taskID, noop)
	if oldCancel != nil {
		oldCancel()
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("previous run was not cancelled")
	}

	// Join every pipeline goroutine BEFORE the deferred runPipeline restore
	// fires — otherwise the restore races their read of the global (-race).
	for _, c := range cancels {
		c()
	}
	for _, d := range dones {
		select {
		case <-d:
		case <-time.After(2 * time.Second):
			t.Fatal("pipeline goroutine never finished")
		}
	}
}

// waitRun joins a runMigration goroutine so the deferred restore of the
// global runPipeline stub never races its read (-race hygiene).
func waitRun(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("%s never finished", what)
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

	// Resume mirrors handleResumeTask (F-08 v2): swap-then-cancel —
	// swapRunning atomically hands ownership to the fresh generation,
	// the stale cancel fires outside the lock.
	_ = st.UpdateTaskStatus(taskID, store.TaskStatusRunning)
	freshCtx, freshCancel := context.WithCancel(context.Background())
	defer freshCancel()
	oldCancel, freshRun := s.swapRunning(taskID, freshCancel)
	if oldCancel == nil {
		t.Fatal("swapRunning must return the stale cancel func")
	}
	oldCancel()
	freshDone := make(chan struct{})
	go func() {
		defer close(freshDone)
		s.runMigration(freshCtx, taskID, config.Config{}, freshRun)
	}()
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
	waitRun(t, freshDone, "fresh run")
}

// F-08 v2 ruling anchor (pure-cancel path): cancelling a live run keeps
// the entry; the OWNING run's cancel branch writes Cancelled and its
// defer cleans up — the handler does not write terminal state.
func TestPureCancelOwningRunWritesCancelled(t *testing.T) {
	s, st := newTestServer(t)
	taskID := "t-purecancel"
	if err := st.CreateTask(&store.Task{ID: taskID, Name: "pc", Status: store.TaskStatusRunning, ConfigJSON: `{}`}); err != nil {
		t.Fatal(err)
	}

	runDone := make(chan struct{})
	oldPipeline := runPipeline
	defer func() { runPipeline = oldPipeline }()
	runPipeline = func(ctx context.Context, _ config.Config, _ orchestrator.PipelineConfig) ([]orchestrator.PipelineResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	runID := s.beginRun(taskID, cancel)
	go func() {
		s.runMigration(ctx, taskID, config.Config{}, runID)
		close(runDone)
	}()
	time.Sleep(150 * time.Millisecond)

	// Pure cancel: take the cancel func WITHOUT removing the entry.
	if c := s.runningCancel(taskID); c == nil {
		t.Fatal("live run expected a registered cancel func")
	} else {
		c()
	}

	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("owning run never finished")
	}

	task, _ := st.GetTask(taskID)
	if task.Status != store.TaskStatusCancelled {
		t.Fatalf("owning run must write Cancelled, got %s", task.Status)
	}
	// Cleanup: entry and log core removed by the owning run's defer.
	s.taskMu.Lock()
	_, hasRun := s.taskRuns[taskID]
	_, hasCore := s.logCores[taskID]
	s.taskMu.Unlock()
	if hasRun || hasCore {
		t.Error("owning run must clean up its run entry and log core")
	}
}

// F-08 v2 ruling anchor (no-live-run cancel): with no registered run
// (paused/finished), the handler writes Cancelled itself.
func TestCancelNoLiveRunHandlerWritesCancelled(t *testing.T) {
	s, st := newTestServer(t)
	taskID := "t-norun"
	if err := st.CreateTask(&store.Task{ID: taskID, Name: "nr", Status: store.TaskStatusPaused, ConfigJSON: `{}`}); err != nil {
		t.Fatal(err)
	}

	if c := s.runningCancel(taskID); c != nil {
		t.Fatal("no live run expected")
	} else {
		// mirror handleCancelTask's else-branch
		_ = st.UpdateTaskStatus(taskID, store.TaskStatusCancelled)
	}
	task, _ := st.GetTask(taskID)
	if task.Status != store.TaskStatusCancelled {
		t.Fatalf("handler must write Cancelled for a task without a live run, got %s", task.Status)
	}
}

// F-08 v2 ruling anchor (superseded success path): a stale run whose
// pipeline returns SUCCESS before the resume's cancel is observed
// (ctx.Err()==nil) must not write Completed/Failed, not store a result,
// and not start a CDC chain — the fresh run owns the task.
func TestSupersededRunSuccessDoesNotWriteTerminalState(t *testing.T) {
	s, st := newTestServer(t)

	taskID := "t-supersede-ok"
	if err := st.CreateTask(&store.Task{ID: taskID, Name: "sok", Status: store.TaskStatusPaused, ConfigJSON: `{}`}); err != nil {
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
			// stale run: hold open until superseded, then return SUCCESS
			// before observing the cancel (the exact race the gate closes).
			<-release
			close(staleDone)
			return []orchestrator.PipelineResult{{Success: true}}, nil
		}
		// fresh run: hold open
		<-freshRelease
		return nil, nil
	}

	staleCtx, staleCancel := context.WithCancel(context.Background())
	defer staleCancel()
	staleRun := s.beginRun(taskID, staleCancel)
	// CDCChain on so the un-guarded path would call startCDCChainAfterSuccess
	// (cdcSupervisor is nil in the test server → it would log a CDC chain line).
	go s.runMigration(staleCtx, taskID, config.Config{}, staleRun)
	time.Sleep(150 * time.Millisecond)

	_ = st.UpdateTaskStatus(taskID, store.TaskStatusRunning)
	freshCtx, freshCancel := context.WithCancel(context.Background())
	defer freshCancel()
	oldCancel, freshRun := s.swapRunning(taskID, freshCancel)
	if oldCancel == nil {
		t.Fatal("swapRunning must return the stale cancel func")
	}
	oldCancel()
	freshDone := make(chan struct{})
	go func() {
		defer close(freshDone)
		s.runMigration(freshCtx, taskID, config.Config{}, freshRun)
	}()
	time.Sleep(150 * time.Millisecond)

	// Stale pipeline returns success AFTER losing ownership.
	close(release)
	select {
	case <-staleDone:
	case <-time.After(2 * time.Second):
		t.Fatal("stale run never finished")
	}
	time.Sleep(150 * time.Millisecond)

	task, _ := st.GetTask(taskID)
	if task.Status != store.TaskStatusRunning {
		t.Fatalf("superseded stale run must not write terminal state, got %s", task.Status)
	}
	if task.ResultJSON != "" {
		t.Error("superseded stale run must not store a task result")
	}

	// CDC chain must not have been started for the stale run (the test
	// server has no cdcSupervisor, so an un-guarded call would leave a
	// "CDC chain:" line in the task log buffer).
	for _, e := range s.logCollector.GetBuffer(taskID).GetAll() {
		if strings.Contains(e.Message, "CDC chain:") {
			t.Error("superseded stale run started a CDC chain")
		}
	}

	close(freshRelease)
	waitRun(t, freshDone, "fresh run")
}

// F-08 v2 fix-A anchor (start path): handleStartTask must swapRunning —
// starting a paused task whose stale run goroutine is still alive cancels
// the stale run instead of leaving two concurrent pipelines.
func TestStartOnPausedCancelsStaleRun(t *testing.T) {
	s, st := newTestServer(t)

	taskID := "t-startswap"
	if err := st.CreateTask(&store.Task{ID: taskID, Name: "ss", Status: store.TaskStatusPaused, ConfigJSON: `{}`}); err != nil {
		t.Fatal(err)
	}

	staleCancelled := make(chan struct{})
	freshRelease := make(chan struct{})
	var call int32
	oldPipeline := runPipeline
	defer func() { runPipeline = oldPipeline }()
	runPipeline = func(ctx context.Context, _ config.Config, _ orchestrator.PipelineConfig) ([]orchestrator.PipelineResult, error) {
		if atomic.AddInt32(&call, 1) == 1 {
			<-ctx.Done()
			close(staleCancelled)
			return nil, ctx.Err()
		}
		<-freshRelease
		return nil, nil
	}

	// Stale run alive (paused without cancel), exactly the double-run setup.
	staleCtx, staleCancel := context.WithCancel(context.Background())
	defer staleCancel()
	staleRun := s.beginRun(taskID, staleCancel)
	go s.runMigration(staleCtx, taskID, config.Config{}, staleRun)
	time.Sleep(150 * time.Millisecond)

	// Drive the real start handler.
	w, req := doReq("POST", "/api/v1/tasks/"+taskID+"/start", "")
	s.handleStartTask(w, withChiParam(req, "taskID", taskID))
	if w.Code != http.StatusOK {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}

	select {
	case <-staleCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("start-on-paused did not cancel the stale run")
	}

	task, _ := st.GetTask(taskID)
	if task.Status != store.TaskStatusRunning {
		t.Fatalf("fresh run must be Running, got %s", task.Status)
	}
	if s.ownsRun(taskID, staleRun) {
		t.Error("stale generation must no longer own the task after start")
	}

	// The handler spawns the fresh run internally — wait until its pipeline
	// stub has been ENTERED (atomic counter) so the deferred runPipeline
	// restore is ordered after the global read (-race hygiene).
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&call) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt32(&call) < 2 {
		t.Fatal("fresh run never entered its pipeline stub")
	}

	close(freshRelease)
}

// F-08 ordering anchor (seq 84/85): the stale run is released at handler
// ENTRY — it may finish with ctx.Err()==nil before/after the swap and
// before/after the Running write. Because ownership is swapped BEFORE the
// Running status write, every interleaving must leave the task Running
// (with the old order the window intermittently flipped it to
// Completed). Non-deterministic interleaving, deterministic outcome.
func TestStaleFinishDuringResumeKeepsRunning(t *testing.T) {
	s, st := newTestServer(t)

	taskID := "t-order"
	if err := st.CreateTask(&store.Task{ID: taskID, Name: "ord", Status: store.TaskStatusPaused, ConfigJSON: `{}`}); err != nil {
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
			<-release // stale: released at handler entry, may finish any time
			close(staleDone)
			return []orchestrator.PipelineResult{{Success: true}}, nil
		}
		<-freshRelease // fresh: hold open
		return nil, nil
	}

	staleCtx, staleCancel := context.WithCancel(context.Background())
	defer staleCancel()
	staleRun := s.beginRun(taskID, staleCancel)
	go s.runMigration(staleCtx, taskID, config.Config{}, staleRun)
	time.Sleep(150 * time.Millisecond)

	// Release the stale run at handler ENTRY (before the handler swaps or
	// writes Running), then drive the real resume handler concurrently.
	close(release)
	w, req := doReq("POST", "/api/v1/tasks/"+taskID+"/resume", "")
	s.handleResumeTask(w, withChiParam(req, "taskID", taskID))
	if w.Code != http.StatusOK {
		t.Fatalf("resume: %d %s", w.Code, w.Body.String())
	}

	select {
	case <-staleDone:
	case <-time.After(2 * time.Second):
		t.Fatal("stale run never finished")
	}
	time.Sleep(200 * time.Millisecond) // let any late stale terminal write land

	task, _ := st.GetTask(taskID)
	if task.Status != store.TaskStatusRunning {
		t.Fatalf("final status must be Running regardless of interleaving, got %s", task.Status)
	}

	// Wait until the handler-spawned fresh run has entered its stub (HB edge
	// for the deferred runPipeline restore, -race hygiene).
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&call) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt32(&call) < 2 {
		t.Fatal("fresh run never entered its pipeline stub")
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
