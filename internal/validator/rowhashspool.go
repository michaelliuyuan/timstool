package validator

// rowhashspool.go — M1 spool: bounded-memory row-hash counting for the
// no-PK/degraded validation paths (hash_group / aggregate / bucket).
//
// Legacy behavior materialized whole tables in memory ([][]string rows or
// map[string]int hash counters, ~50G RSS on a 1e8-row table). The spool
// keeps the EXACT same per-side hash multiset semantics but bounds memory:
//
//   - hashes accumulate in an in-memory map[string]int that is
//     byte-identical to the legacy counters while it fits;
//   - when the buffer crosses maxEntries distinct hashes (~5MB with the
//     64K default) it is sorted and flushed to a disk run (16-byte md5 +
//     uvarint count per distinct hash), and counting continues;
//   - Finish() returns a sorted (hash, count) iterator — a pure
//     in-memory walk when nothing spilled, a cascaded k-way merge
//     (fan-in capped so open fds stay bounded) when runs exist.
//
// Semantics guarantees (M1 red lines):
//   - the multiset of hashes and every derived verdict/message is
//     set-equal to the legacy maps (proven by the oracle equivalence
//     tests in rowhashspool_test.go); detail ordering is now
//     deterministic ascending-hash where the legacy maps iterated in
//     Go-map random order — outputs are byte-identical for ≤1 mismatch
//     by construction and set-equal beyond that;
//   - normalizeValue and the computeRowHash* functions are untouched;
//   - any disk failure fails loud (error wrapped with "row-hash spool"),
//     never silently dropped.
//
// Small tables never touch disk: with defaults the buffer holds 64K
// distinct hashes (~5MB) which covers the overwhelming majority of tables;
// the spool stats stay zero and behavior/perf match the legacy maps.

import (
	"bufio"
	"container/heap"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"
)

const (
	// defaultSpoolRunEntries is the in-memory distinct-hash ceiling before
	// a run is flushed (spec: 64K entries per run, ~5MB of counters).
	defaultSpoolRunEntries = 65536
	// defaultSpoolFanIn caps simultaneously-open run readers during merges
	// so 4 concurrent tables stay well under the fd ulimit.
	defaultSpoolFanIn = 32
	// md5HexLen is the fixed length of the hex md5 hashes spooled.
	md5HexLen = 32
)

// rowHashSpool accumulates per-side row-hash counts with bounded memory,
// spilling sorted runs to disk when the buffer crosses its limits.
type rowHashSpool struct {
	dir        string
	maxEntries int
	fanIn      int

	mem  map[string]int
	runs []string

	rows         uint64 // total Add() calls (the side's row count)
	bytesFlushed int64  // sum of run-file bytes written
	totalFlushed int    // distinct entries flushed (stats only)
	flushedRuns  int
	finished     bool
	closed       bool
	finalIt      hashCountIter // iterator handed out by Finish (Close reclaims)
}

// newRowHashSpool returns a spool writing its run files under dir (which
// must exist). maxEntries <= 0 falls back to the default.
func newRowHashSpool(dir string, maxEntries int) *rowHashSpool {
	if maxEntries <= 0 {
		maxEntries = defaultSpoolRunEntries
	}
	return &rowHashSpool{
		dir:        dir,
		maxEntries: maxEntries,
		fanIn:      defaultSpoolFanIn,
		mem:        make(map[string]int),
	}
}

// Add records one row hash. Memory-identical to the legacy `hashCounts[h]++`
// while the buffer fits; crossing maxEntries triggers a sorted run flush. A
// disk failure is returned loud (callers fail the table).
func (s *rowHashSpool) Add(hash string) error {
	if s.closed {
		return fmt.Errorf("row-hash spool: add after close")
	}
	if s.finished {
		return fmt.Errorf("row-hash spool: add after finish")
	}
	if len(hash) != md5HexLen {
		return fmt.Errorf("row-hash spool: unexpected hash length %d (want md5 hex %d)", len(hash), md5HexLen)
	}
	s.mem[hash]++
	s.rows++
	if len(s.mem) >= s.maxEntries {
		return s.flushRun()
	}
	return nil
}

// flushRun sorts the in-memory counters and writes them as one disk run.
func (s *rowHashSpool) flushRun() error {
	if len(s.mem) == 0 {
		return nil
	}
	keys := make([]string, 0, len(s.mem))
	for k := range s.mem {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	f, err := os.CreateTemp(s.dir, "run-*.spool")
	if err != nil {
		return fmt.Errorf("row-hash spool: create run file: %w", err)
	}
	name := f.Name()
	w := bufio.NewWriterSize(f, 1<<16)
	var written int64
	var vbuf [binary.MaxVarintLen64]byte
	for _, k := range keys {
		var raw [md5HexLen / 2]byte
		if _, err := hex.Decode(raw[:], []byte(k)); err != nil {
			f.Close()
			os.Remove(name)
			return fmt.Errorf("row-hash spool: decode hash %q: %w", k, err)
		}
		if _, err := w.Write(raw[:]); err != nil {
			f.Close()
			os.Remove(name)
			return fmt.Errorf("row-hash spool: write run: %w", err)
		}
		n := binary.PutUvarint(vbuf[:], uint64(s.mem[k]))
		if _, err := w.Write(vbuf[:n]); err != nil {
			f.Close()
			os.Remove(name)
			return fmt.Errorf("row-hash spool: write run count: %w", err)
		}
		written += int64(md5HexLen/2 + n)
	}
	if err := w.Flush(); err != nil {
		f.Close()
		os.Remove(name)
		return fmt.Errorf("row-hash spool: flush run: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("row-hash spool: close run: %w", err)
	}

	s.totalFlushed += len(keys)
	s.flushedRuns++
	s.bytesFlushed += written
	s.runs = append(s.runs, name)
	s.mem = make(map[string]int)
	return nil
}

// Finish seals the spool and returns a sorted (hash, count) iterator.
// Nothing spilled → a pure in-memory walk (zero disk). Spilled → the
// remaining mem is flushed and runs are cascade-merged down to <= fanIn
// open files before the final streaming merge.
func (s *rowHashSpool) Finish() (hashCountIter, error) {
	if s.closed {
		return nil, fmt.Errorf("row-hash spool: finish after close")
	}
	if s.finished {
		return nil, fmt.Errorf("row-hash spool: finish twice")
	}
	s.finished = true

	if len(s.runs) == 0 {
		keys := make([]string, 0, len(s.mem))
		for k := range s.mem {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		counts := make([]int, len(keys))
		for i, k := range keys {
			counts[i] = s.mem[k]
		}
		return &memHashCountIter{keys: keys, counts: counts}, nil
	}

	if err := s.flushRun(); err != nil {
		return nil, err
	}

	runs := append([]string(nil), s.runs...)
	for len(runs) > s.fanIn {
		var next []string
		var consumed []string
		var created []string // merge files produced by THIS pass only
		for i := 0; i < len(runs); i += s.fanIn {
			group := runs[i:min(i+s.fanIn, len(runs))]
			if len(group) == 1 {
				// Tail remainder: carried over untouched — it is still a
				// live input of a later pass/final merge, so it must NOT
				// be deleted with this pass's consumed inputs (review
				// probe TestReviewCascadeSingleRemainder anchor).
				next = append(next, group...)
				continue
			}
			merged, err := s.mergeRuns(group)
			if err != nil {
				// Drop the merges this pass produced; carried-over runs
				// stay owned by s.runs (Close()/Run's RemoveAll clean up).
				for _, m := range created {
					os.Remove(m)
				}
				return nil, err
			}
			next = append(next, merged)
			created = append(created, merged)
			consumed = append(consumed, group...)
		}
		for _, old := range consumed {
			os.Remove(old)
		}
		runs = next
	}
	s.runs = runs

	iters, err := s.openRunReaders(runs)
	if err != nil {
		return nil, err
	}
	it := newSpoolMergeIter(iters)
	s.finalIt = it
	return it, nil
}

// mergeRuns k-way merges group into ONE new sorted run (counts of equal
// hashes summed) and returns its path.
func (s *rowHashSpool) mergeRuns(group []string) (string, error) {
	iters, err := s.openRunReaders(group)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(s.dir, "merge-*.spool")
	if err != nil {
		closeRunIters(iters)
		return "", fmt.Errorf("row-hash spool: create merge file: %w", err)
	}
	name := f.Name()
	w := bufio.NewWriterSize(f, 1<<16)
	var vbuf [binary.MaxVarintLen64]byte
	mi := newSpoolMergeIter(iters)
	for {
		h, c, err := mi.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			f.Close()
			os.Remove(name)
			mi.Close()
			return "", err
		}
		var raw [md5HexLen / 2]byte
		if _, derr := hex.Decode(raw[:], []byte(h)); derr != nil {
			f.Close()
			os.Remove(name)
			mi.Close()
			return "", fmt.Errorf("row-hash spool: decode hash: %w", derr)
		}
		if _, werr := w.Write(raw[:]); werr != nil {
			f.Close()
			os.Remove(name)
			mi.Close()
			return "", fmt.Errorf("row-hash spool: write merge: %w", werr)
		}
		n := binary.PutUvarint(vbuf[:], uint64(c))
		if _, werr := w.Write(vbuf[:n]); werr != nil {
			f.Close()
			os.Remove(name)
			mi.Close()
			return "", fmt.Errorf("row-hash spool: write merge count: %w", werr)
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		os.Remove(name)
		mi.Close()
		return "", fmt.Errorf("row-hash spool: flush merge: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		mi.Close()
		return "", fmt.Errorf("row-hash spool: close merge: %w", err)
	}
	mi.Close()
	return name, nil
}

func (s *rowHashSpool) openRunReaders(runs []string) ([]hashCountIter, error) {
	iters := make([]hashCountIter, 0, len(runs))
	for _, r := range runs {
		f, err := os.Open(r)
		if err != nil {
			closeRunIters(iters)
			return nil, fmt.Errorf("row-hash spool: open run: %w", err)
		}
		iters = append(iters, newRunFileIter(f))
	}
	return iters, nil
}

// Close releases everything: the iterator handed out by Finish (its run
// readers must be closed before the files can be removed on every OS),
// then every run/merge file this spool created. Idempotent.
func (s *rowHashSpool) Close() {
	if s.closed {
		return
	}
	s.closed = true
	if s.finalIt != nil {
		s.finalIt.Close()
		s.finalIt = nil
	}
	for _, n := range s.runs {
		os.Remove(n)
	}
	s.runs = nil
	s.mem = nil
}

// Rows reports total Add() calls (the side's row count).
func (s *rowHashSpool) Rows() uint64 { return s.rows }

// Spilled reports whether any run hit disk.
func (s *rowHashSpool) Spilled() bool { return s.flushedRuns > 0 }

// StatsLabel renders this side's spool stats ("N runs/X.XMB disk"), empty
// when the side never touched disk.
func (s *rowHashSpool) StatsLabel() string {
	if !s.Spilled() {
		return ""
	}
	return fmt.Sprintf("%d runs/%.1fMB", s.flushedRuns, float64(s.bytesFlushed)/1e6)
}

// spoolSuffixFor renders the combined per-table disclosure appended to a
// Suggestion when (and only when) either side actually hit disk.
func spoolSuffixFor(src, tgt *rowHashSpool) string {
	if !src.Spilled() && !tgt.Spilled() {
		return ""
	}
	srcLabel := src.StatsLabel()
	if srcLabel == "" {
		srcLabel = "0 runs"
	}
	tgtLabel := tgt.StatsLabel()
	if tgtLabel == "" {
		tgtLabel = "0 runs"
	}
	return fmt.Sprintf(" (row-hash spool: src %s, tidb %s)", srcLabel, tgtLabel)
}

// hashCountIter yields (hash, count) pairs in strictly ascending hash
// order; io.EOF marks the end. Implementations must be consumed by a
// single goroutine.
type hashCountIter interface {
	Next() (hash string, count int, err error)
	Close() error
}

// memHashCountIter walks the sorted in-memory counters (no disk at all).
type memHashCountIter struct {
	keys   []string
	counts []int
	pos    int
}

func (m *memHashCountIter) Next() (string, int, error) {
	if m.pos >= len(m.keys) {
		return "", 0, io.EOF
	}
	k := m.keys[m.pos]
	c := m.counts[m.pos]
	m.pos++
	return k, c, nil
}
func (m *memHashCountIter) Close() error { return nil }

// runFileIter reads one flushed run: 16-byte md5 + uvarint count records.
type runFileIter struct {
	f  *os.File
	br *bufio.Reader
}

func newRunFileIter(f *os.File) *runFileIter {
	return &runFileIter{f: f, br: bufio.NewReaderSize(f, 1<<16)}
}

func (r *runFileIter) Next() (string, int, error) {
	var raw [md5HexLen / 2]byte
	if _, err := io.ReadFull(r.br, raw[:]); err != nil {
		if err == io.EOF {
			return "", 0, io.EOF
		}
		return "", 0, fmt.Errorf("row-hash spool: read run hash: %w", err)
	}
	c, err := binary.ReadUvarint(r.br)
	if err != nil {
		return "", 0, fmt.Errorf("row-hash spool: read run count: %w", err)
	}
	return hex.EncodeToString(raw[:]), int(c), nil
}

func (r *runFileIter) Close() error { return r.f.Close() }

// spoolMergeIter k-way merges child iterators, summing counts of equal
// hashes, yielding strictly ascending (hash, summedCount) pairs.
type spoolMergeIter struct {
	kids  []hashCountIter
	h     mergeHeap
	err   error
	done  bool
	start bool
}

func newSpoolMergeIter(kids []hashCountIter) *spoolMergeIter {
	mi := &spoolMergeIter{kids: kids}
	mi.h = make(mergeHeap, 0, len(kids))
	for i, k := range kids {
		hash, count, err := k.Next()
		if err == io.EOF {
			continue
		}
		if err != nil {
			mi.err = err
			break
		}
		mi.h = append(mi.h, &mergeItem{hash: hash, count: count, idx: i})
	}
	heap.Init(&mi.h)
	return mi
}

type mergeItem struct {
	hash  string
	count int
	idx   int
}

type mergeHeap []*mergeItem

func (h mergeHeap) Len() int            { return len(h) }
func (h mergeHeap) Less(i, j int) bool  { return h[i].hash < h[j].hash }
func (h mergeHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *mergeHeap) Push(x interface{}) { *h = append(*h, x.(*mergeItem)) }
func (h *mergeHeap) Pop() interface{} {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

func (mi *spoolMergeIter) Next() (string, int, error) {
	if mi.err != nil {
		return "", 0, mi.err
	}
	if !mi.start {
		mi.start = true
	}
	if mi.done || mi.h.Len() == 0 {
		mi.done = true
		return "", 0, io.EOF
	}
	top := mi.h[0]
	outHash := top.hash
	outCount := 0
	for mi.h.Len() > 0 && mi.h[0].hash == outHash {
		item := mi.h[0]
		outCount += item.count
		nh, nc, err := mi.kids[item.idx].Next()
		if err == io.EOF {
			heap.Pop(&mi.h)
			continue
		}
		if err != nil {
			mi.err = err
			return "", 0, err
		}
		item.hash = nh
		item.count = nc
		heap.Fix(&mi.h, 0)
	}
	return outHash, outCount, nil
}

func (mi *spoolMergeIter) Close() error {
	var first error
	for _, k := range mi.kids {
		if err := k.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func closeRunIters(iters []hashCountIter) {
	for _, it := range iters {
		it.Close()
	}
}

// walkHashCounters walks two sorted (hash, count) iterators in lockstep
// and invokes onPair for EVERY distinct hash present on either side (a
// missing side is reported as count 0). This subsumes the multiset
// comparison the legacy per-side map lookups performed: two sides are
// equal iff no pair has differing counts. Streaming and deterministic
// (ascending hash order).
func walkHashCounters(a, b hashCountIter, onPair func(hash string, ca, cb int)) error {
	ha, ca, erra := a.Next()
	hb, cb, errb := b.Next()
	for {
		if erra == io.EOF && errb == io.EOF {
			return nil
		}
		if erra != nil && erra != io.EOF {
			return erra
		}
		if errb != nil && errb != io.EOF {
			return errb
		}
		switch {
		case errb == io.EOF || (erra != io.EOF && ha < hb):
			onPair(ha, ca, 0)
			ha, ca, erra = a.Next()
		case erra == io.EOF || hb < ha:
			onPair(hb, 0, cb)
			hb, cb, errb = b.Next()
		default: // ha == hb
			onPair(ha, ca, cb)
			ha, ca, erra = a.Next()
			hb, cb, errb = b.Next()
		}
	}
}
