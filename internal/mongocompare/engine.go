// Package mongocompare is the MongoDB↔MongoDB consistency-compare engine
// (M2): a Go port of mongo-dbcheck's js/cli.js per docs/canonical-bson-spec.
// Per-collection flow: existence → TTL probe (ttl collections compare index
// level only and are skipped for data) → index set → exact count → bucketed
// streaming hashes (client-side canonicalization, spec §9) → merge-join
// drill-down on mismatched buckets only. Bucket results checkpoint to JSON
// for resume; boundaries are recomputed every run (P1 fix: never persist BSON
// boundaries).
package mongocompare

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/michaelliuyuan/timstool/internal/mongocompare/canon"
	"github.com/michaelliuyuan/timstool/internal/mongoconn"
	"github.com/michaelliuyuan/timstool/internal/source"
)

// Level is one comparison stage.
type Level string

const (
	LevelSchema Level = "schema" // collection existence
	LevelIndex  Level = "index"
	LevelCount  Level = "count"
	LevelHash   Level = "hash"
	LevelDiff   Level = "diff"
)

// Config mirrors the js CLI knobs. Zero values default (Buckets 64,
// parallelism 4/4, batch 1000, all levels).
type Config struct {
	Buckets         int
	ParallelColls   int
	ParallelBuckets int
	BatchSize       int
	Levels          []Level
	Opts            canon.Opts
	Resume          bool
	OutDir          string // report dir: summary.json, diff/*.ndjson, .checkpoints/*.json
}

func (c Config) withDefaults() Config {
	if c.Buckets <= 0 {
		c.Buckets = 64
	}
	if c.ParallelColls <= 0 {
		c.ParallelColls = 4
	}
	if c.ParallelBuckets <= 0 {
		c.ParallelBuckets = 4
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 1000
	}
	if len(c.Levels) == 0 {
		c.Levels = []Level{LevelSchema, LevelIndex, LevelCount, LevelHash, LevelDiff}
	}
	return c
}

func (c Config) has(l Level) bool {
	for _, x := range c.Levels {
		if x == l {
			return true
		}
	}
	return false
}

// Overall values (spec §13 three-state + ttl_skip).
const (
	OverallEqual   = "equal"
	OverallDiff    = "diff"
	OverallError   = "error"
	OverallTTLSkip = "ttl_skip"
)

// BucketResult is one bucket's comparison outcome (also the checkpoint unit).
type BucketResult struct {
	Bucket   int    `json:"bucket"`
	Match    bool   `json:"match"`
	SrcCount int64  `json:"src_count"`
	DstCount int64  `json:"dst_count"`
	SrcHash  string `json:"src_hash"`
	DstHash  string `json:"dst_hash"`
}

// CollectionResult is one collection's comparison outcome.
type CollectionResult struct {
	DB      string `json:"db"`
	Coll    string `json:"coll"`
	Overall string `json:"overall"`
	Checks  struct {
		Schema  map[string]any `json:"schema,omitempty"`
		Index   map[string]any `json:"index,omitempty"`
		Count   map[string]any `json:"count,omitempty"`
		Hash    []BucketResult `json:"hash,omitempty"`
		Diff    map[string]any `json:"diff,omitempty"`
		Skipped string         `json:"skipped,omitempty"`
		Error   string         `json:"error,omitempty"`
	} `json:"checks"`
}

// Summary is the whole-run outcome (summary.json shape).
type Summary struct {
	StartedAt   time.Time          `json:"started_at"`
	FinishedAt  time.Time          `json:"finished_at"`
	DurationSec float64            `json:"duration_sec"`
	Collections []CollectionResult `json:"collections"`
	Counts      map[string]int     `json:"counts"`
	Overall     string             `json:"overall"`
	Stats       map[string]any     `json:"stats"`
}

// CompareClients enumerates the source client's user databases (skipping
// admin/config/local and system.* collections) and compares each collection
// against the same-named database on the target client.
func CompareClients(ctx context.Context, src, dst mongoconn.Client, cfg Config) (*Summary, error) {
	c := cfg.withDefaults()
	s := newSummary(c)
	if err := prepOutDir(c); err != nil {
		return nil, err
	}

	dbs, err := src.ListDatabases(ctx)
	if err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}
	var all []CollectionResult
	for _, d := range dbs {
		if d.Name == "admin" || d.Name == "config" || d.Name == "local" {
			continue
		}
		rows, err := compareDB(ctx, src, dst, d.Name, d.Name, c)
		if err != nil {
			return nil, err
		}
		all = append(all, rows...)
	}
	finalize(s, all)
	writeSummary(c, s)
	return s, nil
}

// CompareDatabases compares every collection of srcDB against dstDB (the
// pair may live on the same client — the single-instance test path — or on
// two clients with different names, e.g. a task's source/target mapping).
func CompareDatabases(ctx context.Context, src, dst mongoconn.Client, srcDB, dstDB string, cfg Config) (*Summary, error) {
	c := cfg.withDefaults()
	s := newSummary(c)
	if err := prepOutDir(c); err != nil {
		return nil, err
	}
	rows, err := compareDB(ctx, src, dst, srcDB, dstDB, c)
	if err != nil {
		return nil, err
	}
	finalize(s, rows)
	writeSummary(c, s)
	return s, nil
}

func newSummary(c Config) *Summary {
	return &Summary{StartedAt: time.Now(), Overall: OverallEqual,
		Counts: map[string]int{"equal": 0, "diff": 0, "error": 0, "ttl_skip": 0},
		Stats:  map[string]any{"buckets": c.Buckets, "parallel_colls": c.ParallelColls, "parallel_buckets": c.ParallelBuckets, "batch_size": c.BatchSize, "resume": c.Resume}}
}

func prepOutDir(c Config) error {
	if c.OutDir == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(c.OutDir, "diff"), 0o755); err != nil {
		return err
	}
	return os.MkdirAll(filepath.Join(c.OutDir, ".checkpoints"), 0o755)
}

func writeSummary(c Config, s *Summary) {
	if c.OutDir == "" {
		return
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	_ = os.WriteFile(filepath.Join(c.OutDir, "summary.json"), b, 0o644)
}

func finalize(s *Summary, rows []CollectionResult) {
	s.Collections = rows
	for _, r := range rows {
		switch r.Overall {
		case OverallTTLSkip:
			s.Counts["ttl_skip"]++
		case OverallDiff:
			s.Counts["diff"]++
		case OverallError:
			s.Counts["error"]++
		default:
			s.Counts["equal"]++
		}
	}
	if s.Counts["diff"] > 0 {
		s.Overall = OverallDiff
	} else if s.Counts["error"] > 0 {
		s.Overall = OverallError
	}
	s.FinishedAt = time.Now()
	s.DurationSec = time.Since(s.StartedAt).Seconds()
}

// compareDB enumerates srcDB's non-system collections and compares each one
// against the same-named collection in dstDB.
func compareDB(ctx context.Context, src, dst mongoconn.Client, srcDB, dstDB string, c Config) ([]CollectionResult, error) {
	names, err := src.ListCollectionNames(ctx, srcDB)
	if err != nil {
		return nil, fmt.Errorf("list collections %s: %w", srcDB, err)
	}
	type task struct{ db, coll string }
	var tasks []task
	for _, name := range names {
		if strings.HasPrefix(name, "system.") {
			continue
		}
		tasks = append(tasks, task{srcDB, name})
	}

	results := make([]CollectionResult, len(tasks))
	sem := make(chan struct{}, c.ParallelColls)
	var wg sync.WaitGroup
	for i, t := range tasks {
		wg.Add(1)
		go func(i int, t task) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = compareCollection(ctx, src, dst, t.db, dstDB, t.coll, c)
		}(i, t)
	}
	wg.Wait()
	return results, nil
}

// AlgoVersion stamps the canon semantics/format a checkpoint was produced
// with (M2 arbitration seq=48/49/50): any change to canonical serialization,
// hashing, or bucket-boundary arithmetic increments it; checkpoints with a
// different version are discarded and fully recomputed (no migration).
const AlgoVersion = 1

// checkpoint file format (bucket results + canonical boundary strings —
// M2 arbitration seq=50 requires hash AND boundaries to coexist; boundaries
// are stored as canon strings, never raw BSON, so they cannot distort on
// deserialize. A stored bucket whose boundaries no longer match the
// recomputed plan is treated as stale and recomputed — data-shift protection
// beyond the js P1 fix).
type checkpoint struct {
	AlgoVersion int              `json:"algo_version"`
	Buckets     map[int]cpBucket `json:"buckets"`
	Done        bool             `json:"done"`
}

type cpBucket struct {
	Bucket   int    `json:"bucket"`
	Match    bool   `json:"match"`
	SrcCount int64  `json:"src_count"`
	DstCount int64  `json:"dst_count"`
	SrcHash  string `json:"src_hash"`
	DstHash  string `json:"dst_hash"`
	LoCanon  string `json:"lo_canon,omitempty"`
	HiCanon  string `json:"hi_canon,omitempty"`
}

func cpPath(outDir, db, coll string) string {
	return filepath.Join(outDir, ".checkpoints", strings.ReplaceAll(db+"."+coll, ".", "__")+".json")
}

func loadCheckpoint(cfg Config, db, coll string) checkpoint {
	cp := checkpoint{AlgoVersion: AlgoVersion, Buckets: map[int]cpBucket{}}
	if !cfg.Resume || cfg.OutDir == "" {
		return cp
	}
	b, err := os.ReadFile(cpPath(cfg.OutDir, db, coll))
	if err != nil {
		return cp
	}
	_ = json.Unmarshal(b, &cp)
	if cp.AlgoVersion != AlgoVersion {
		// Stale semantics: discard, recompute everything (no migration).
		return checkpoint{AlgoVersion: AlgoVersion, Buckets: map[int]cpBucket{}}
	}
	if cp.Buckets == nil {
		cp.Buckets = map[int]cpBucket{}
	}
	return cp
}

func (cp *checkpoint) save(cfg Config, db, coll string) {
	if cfg.OutDir == "" {
		return
	}
	b, _ := json.MarshalIndent(cp, "", "  ")
	_ = os.WriteFile(cpPath(cfg.OutDir, db, coll), b, 0o644)
}

// compareCollection compares one collection: src/srcDB against dst/dstDB.
// Works through the version-adaptive connection (M5) — both driver paths
// produce the same neutral index/count/cursor surfaces.
func compareCollection(ctx context.Context, src, dst mongoconn.Client, srcDB, dstDB, coll string, cfg Config) CollectionResult {
	var r CollectionResult
	r.DB, r.Coll, r.Overall = srcDB, coll, OverallEqual
	sc := src.Collection(srcDB, coll)
	tc := dst.Collection(dstDB, coll)

	defer func() {
		if rec := recover(); rec != nil {
			r.Overall = OverallError
			r.Checks.Error = fmt.Sprintf("panic: %v", rec)
		}
	}()

	// Schema: existence on target.
	if cfg.has(LevelSchema) {
		names, err := dst.ListCollectionNames(ctx, dstDB)
		if err != nil {
			r.Overall, r.Checks.Error = OverallError, err.Error()
			return r
		}
		found := false
		for _, n := range names {
			if n == coll {
				found = true
				break
			}
		}
		if !found {
			r.Overall = OverallDiff
			r.Checks.Schema = map[string]any{"diff": []string{"missing-in-target"}}
			return r
		}
		r.Checks.Schema = map[string]any{"ok": true}
	}

	// TTL probe + index level.
	srcIdx, err := sc.IndexSpecs(ctx)
	if err != nil {
		r.Overall, r.Checks.Error = OverallError, err.Error()
		return r
	}
	hasTTL := false
	for _, ix := range srcIdx {
		if ix.ExpireAfterSeconds != nil {
			hasTTL = true
			break
		}
	}
	if cfg.has(LevelIndex) {
		dstIdx, err := tc.IndexSpecs(ctx)
		if err != nil {
			r.Overall, r.Checks.Error = OverallError, err.Error()
			return r
		}
		d := diffIndexSets(srcIdx, dstIdx)
		if len(d) > 0 {
			r.Checks.Index = map[string]any{"diff": d}
			if r.Overall == OverallEqual {
				r.Overall = OverallDiff
			}
		} else {
			r.Checks.Index = map[string]any{"ok": true}
		}
	}
	if hasTTL {
		r.Overall = OverallTTLSkip
		r.Checks.Skipped = "ttl"
		return r
	}

	if cfg.has(LevelCount) {
		scN, err1 := sc.CountDocuments(ctx)
		tcN, err2 := tc.CountDocuments(ctx)
		if err1 != nil || err2 != nil {
			r.Overall, r.Checks.Error = OverallError, firstErr(err1, err2)
			return r
		}
		r.Checks.Count = map[string]any{"src": scN, "dst": tcN, "ok": scN == tcN}
		if scN != tcN {
			r.Overall = OverallDiff
		}
	}

	cp := loadCheckpoint(cfg, srcDB, coll)
	if cfg.has(LevelHash) || cfg.has(LevelDiff) {
		rangeStats, err := sc.IDRange(ctx)
		if err != nil {
			r.Overall, r.Checks.Error = OverallError, err.Error()
			return r
		}
		if rangeStats.N == 0 {
			cp.Done = true
			cp.save(cfg, srcDB, coll)
			r.Checks.Hash = []BucketResult{}
		} else {
			plan := PlanBuckets(rangeStats.Min, rangeStats.Max, cfg.Buckets)
			results := make([]BucketResult, len(plan))
			sem := make(chan struct{}, cfg.ParallelBuckets)
			var wg sync.WaitGroup
			var mu sync.Mutex
			for i, b := range plan {
				wg.Add(1)
				go func(i int, b BucketPlan) {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					loCanon := canon.Canonical(b.Lo, cfg.Opts)
					hiCanon := canon.Canonical(b.Hi, cfg.Opts)
					// Checkpoint reuse read takes the same mutex as the write
					// below (concurrent buckets of one collection share cp —
					// an unlocked read races the writer goroutine; caught by
					// the M5 bridge-equivalence run, latent since M2).
					mu.Lock()
					prev, reusable := cp.Buckets[b.Index]
					if reusable {
						reusable = prev.LoCanon == loCanon && prev.HiCanon == hiCanon
					}
					mu.Unlock()
					if reusable {
						// Reusable: same semantics version AND same boundaries
						// as the recomputed plan (data-shift protection).
						results[i] = BucketResult{
							Bucket: prev.Bucket, Match: prev.Match,
							SrcCount: prev.SrcCount, DstCount: prev.DstCount,
							SrcHash: prev.SrcHash, DstHash: prev.DstHash,
						}
						return
					}
					srcD, err1 := HashSide(ctx, sc, b.Query, cfg.Opts, cfg.BatchSize)
					dstD, err2 := HashSide(ctx, tc, b.Query, cfg.Opts, cfg.BatchSize)
					if err1 != nil || err2 != nil {
						mu.Lock()
						results[i] = BucketResult{Bucket: b.Index, Match: false}
						r.Overall = OverallError
						r.Checks.Error = firstErr(err1, err2)
						mu.Unlock()
						return
					}
					br := BucketResult{
						Bucket:   b.Index,
						Match:    srcD.Hash == dstD.Hash && srcD.Count == dstD.Count,
						SrcCount: srcD.Count, DstCount: dstD.Count,
						SrcHash: srcD.Hash, DstHash: dstD.Hash,
					}
					results[i] = br
					mu.Lock()
					cp.Buckets[b.Index] = cpBucket{
						Bucket: br.Bucket, Match: br.Match,
						SrcCount: br.SrcCount, DstCount: br.DstCount,
						SrcHash: br.SrcHash, DstHash: br.DstHash,
						LoCanon: loCanon, HiCanon: hiCanon,
					}
					cp.save(cfg, srcDB, coll)
					mu.Unlock()
				}(i, b)
			}
			wg.Wait()
			r.Checks.Hash = results
			for _, br := range results {
				if !br.Match {
					if r.Overall == OverallError {
						break
					}
					r.Overall = OverallDiff
				}
			}
		}
	}

	if cfg.has(LevelDiff) {
		var diffBuckets []BucketPlan
		if r.Checks.Hash != nil {
			byIdx := map[int]bool{}
			for _, br := range r.Checks.Hash {
				if !br.Match {
					byIdx[br.Bucket] = true
				}
			}
			rangeStats, err := sc.IDRange(ctx)
			if err == nil && rangeStats.N > 0 {
				for _, b := range PlanBuckets(rangeStats.Min, rangeStats.Max, cfg.Buckets) {
					if byIdx[b.Index] {
						diffBuckets = append(diffBuckets, b)
					}
				}
			}
		}
		var deltas []DocDelta
		for _, b := range diffBuckets {
			ds, err := DiffBucket(ctx, sc, tc, b.Query, cfg.Opts, cfg.BatchSize)
			if err != nil {
				r.Overall, r.Checks.Error = OverallError, err.Error()
				return r
			}
			deltas = append(deltas, ds...)
		}
		if len(deltas) > 0 {
			if r.Overall == OverallEqual {
				r.Overall = OverallDiff
			}
			r.Checks.Diff = map[string]any{"count": len(deltas)}
		} else if r.Checks.Diff == nil && len(deltas) == 0 && r.Overall != OverallError {
			r.Checks.Diff = map[string]any{"count": 0}
		}
		writeDeltas(cfg, srcDB, coll, deltas)
	}

	cp.Done = true
	cp.save(cfg, srcDB, coll)
	return r
}

func writeDeltas(cfg Config, db, coll string, deltas []DocDelta) {
	if cfg.OutDir == "" {
		return
	}
	name := strings.ReplaceAll(db+"."+coll, ".", "__") + ".ndjson"
	var b strings.Builder
	for _, d := range deltas {
		j, err := json.Marshal(d)
		if err != nil {
			continue
		}
		b.Write(j)
		b.WriteByte('\n')
	}
	_ = os.WriteFile(filepath.Join(cfg.OutDir, "diff", name), []byte(b.String()), 0o644)
}

// diffIndexSets compares the normalized index summaries of both sides. The
// js normIdx dropped v/ns from the raw index documents; the Go port builds a
// canonical string from the neutral source.IndexInfo (name, ordered key
// document, unique/sparse/clustered/TTL), which is version-stable and
// driver-agnostic (M5: identical on the v2 and v1-bridge paths).
func diffIndexSets(src, dst []source.IndexInfo) []string {
	norm := func(specs []source.IndexInfo) map[string]bool {
		m := make(map[string]bool, len(specs))
		for _, ix := range specs {
			m[indexCanon(ix)] = true
		}
		return m
	}
	sa, sb := norm(src), norm(dst)
	var out []string
	for k := range sa {
		if !sb[k] {
			out = append(out, "missing-in-target: "+k)
		}
	}
	for k := range sb {
		if !sa[k] {
			out = append(out, "missing-in-source: "+k)
		}
	}
	return out
}

func indexCanon(ix source.IndexInfo) string {
	d := bson.D{{Key: "name", Value: ix.Name}}
	if len(ix.Keys) > 0 {
		key := make(bson.D, 0, len(ix.Keys))
		for _, k := range ix.Keys {
			key = append(key, bson.E{Key: k.Field, Value: k.Order})
		}
		d = append(d, bson.E{Key: "key", Value: key})
	}
	if ix.Unique {
		d = append(d, bson.E{Key: "unique", Value: true})
	}
	if ix.ExpireAfterSeconds != nil {
		d = append(d, bson.E{Key: "expireAfterSeconds", Value: *ix.ExpireAfterSeconds})
	}
	if ix.Sparse {
		d = append(d, bson.E{Key: "sparse", Value: true})
	}
	if ix.Clustered {
		d = append(d, bson.E{Key: "clustered", Value: true})
	}
	return canon.Canonical(d, canon.Opts{})
}

func firstErr(errs ...error) string {
	for _, e := range errs {
		if e != nil {
			return e.Error()
		}
	}
	return "unknown error"
}
