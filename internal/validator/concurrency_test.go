package validator

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/michaelliuyuan/timstool/internal/common/config"
)

// #t4 anchors: unified concurrency budget resolution (legacy knob folding +
// clamp + default), ctx-cancellable semaphore acquisition, and peak-slot
// accounting under contention.

// A1: ResolveConcurrency — explicit wins; legacy knobs fold via max; default
// 4; hard clamp at MaxConcBudget (8).
func TestResolveConcurrency(t *testing.T) {
	cases := []struct {
		conc, par, chunk, want int
	}{
		{0, 0, 0, 4},    // nothing set → default
		{6, 2, 3, 6},    // explicit wins over both legacy knobs
		{0, 2, 3, 3},    // legacy max(chunk) wins
		{0, 5, 2, 5},    // legacy max(parallel) wins
		{0, 1, 1, 1},    // lower bound
		{0, 32, 0, 8},   // legacy over cap clamps
		{99, 0, 0, 8},   // explicit over cap clamps
		{8, 1, 1, 8},    // explicit at cap kept
		{-1, 0, 0, 4},   // garbage → default
	}
	for i, c := range cases {
		if got := ResolveConcurrency(c.conc, c.par, c.chunk); got != c.want {
			t.Errorf("case %d: ResolveConcurrency(%d,%d,%d)=%d want %d", i, c.conc, c.par, c.chunk, got, c.want)
		}
	}
}

// A2: validator.concurrency — CompareConfig.Concurrency (override path used
// by webapi compare tasks) beats the parallel override and config-file knobs.
func TestValidatorConcurrency(t *testing.T) {
	v := NewValidator(config.Config{})
	v.compareOverride = &config.CompareConfig{Concurrency: 3}
	v.parallelOverride = 7
	if got := v.concurrency(); got != 3 {
		t.Fatalf("compareCfg.Concurrency must win, got %d", got)
	}
	v2 := NewValidator(config.Config{})
	v2.compareOverride = &config.CompareConfig{ChecksumParallel: 6}
	v2.parallelOverride = 2
	if got := v2.concurrency(); got != 6 {
		t.Fatalf("legacy chunk parallel must fold via max, got %d", got)
	}
}

// A3: concSem.acquire honors ctx cancellation — a goroutine blocked on a
// full sem must return when the context is cancelled (the "cancel 无效"
// half of the #t4 deadlock bug).
func TestConcSemAcquireCancel(t *testing.T) {
	sem := make(concSem, 1)
	release, err := sem.acquire(context.Background())
	if err != nil {
		t.Fatalf("first acquire must succeed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := sem.acquire(ctx)
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked acquire must fail on ctx cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acquire did not observe cancellation")
	}
	release()
}

// A4: peak-slot accounting — with a budget of B and many concurrent workers,
// the number of simultaneously held slots never exceeds B, and all workers
// finish (no deadlock, no slot leak).
func TestConcSemPeakWithinBudget(t *testing.T) {
	const budget = 4
	const workers = 64
	sem := make(concSem, budget)

	var cur, peak int32
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := sem.acquire(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			c := atomic.AddInt32(&cur, 1)
			for {
				p := atomic.LoadInt32(&peak)
				if c <= p || atomic.CompareAndSwapInt32(&peak, p, c) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			atomic.AddInt32(&cur, -1)
			release()
		}()
	}
	wg.Wait()
	if p := atomic.LoadInt32(&peak); p > budget {
		t.Fatalf("peak concurrent slots %d exceeded budget %d", p, budget)
	}
	if p := atomic.LoadInt32(&peak); p == 0 {
		t.Fatal("test sanity: peak must be positive")
	}
}
