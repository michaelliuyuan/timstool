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

	"github.com/michaelliuyuan/timstool/internal/common/config"
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
	// 第9单 ①: optional watermark window (the SAME filter group as the
	// pg/mysql compare side — config.WatermarkFilter is dialect-free).
	// nil/inactive = full comparison, byte-identical legacy behavior.
	Watermark       *config.WatermarkFilter
	TableWatermarks []config.TableWatermark
	// coll-select ①: optional collection whitelist scoped to the task's
	// source database. nil/empty = enumerate everything (legacy semantics,
	// byte-identical). Only the single-database task chain sets it; the
	// all-instance CLI path (CompareClients) never does.
	OnlyCollections []string
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
	DB   string `json:"db"`
	Coll string `json:"coll"`
	// 第9单 ⑦: watermark disclosure — WmState ∈ windowed/degraded/failed
	// ("" = watermark not applicable/off) and Note carries the human
	// wording (scope note or degradation disclosure), mirroring the
	// pg/mysql per-table note.
	WmState string `json:"wm_state,omitempty"`
	Note    string `json:"note,omitempty"`
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
	var skippedViews []string
	for _, d := range dbs {
		if d.Name == "admin" || d.Name == "config" || d.Name == "local" {
			continue
		}
		rows, skipped, _, err := compareDB(ctx, src, dst, d.Name, d.Name, c)
		if err != nil {
			return nil, err
		}
		all = append(all, rows...)
		skippedViews = append(skippedViews, skipped...)
	}
	finalize(s, all)
	recordSkippedViews(s, skippedViews)
	if err := writeSummary(c, s); err != nil {
		return nil, err
	}
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
	rows, skipped, picked, err := compareDB(ctx, src, dst, srcDB, dstDB, c)
	if err != nil {
		return nil, err
	}
	// coll-select ①: a whitelist where NOT ONE selected collection exists
	// in the source database (typo, or the collection was dropped after
	// task creation) must fail loudly — a silent "completed, 0 compared"
	// would read as equality evidence.
	if len(c.OnlyCollections) > 0 && picked == 0 {
		return nil, fmt.Errorf("none of the %d selected collections exist in source database %q", len(c.OnlyCollections), srcDB)
	}
	if len(c.OnlyCollections) > 0 {
		// Disclosure pair: selected (what the user asked for) vs matched
		// (what the source still had). Divergence = post-creation drift,
		// surfaced by the report face instead of failing the whole run.
		s.Stats["selected_collections"] = len(c.OnlyCollections)
		s.Stats["matched_collections"] = picked
	}
	finalize(s, rows)
	recordSkippedViews(s, skipped)
	if err := writeSummary(c, s); err != nil {
		return nil, err
	}
	return s, nil
}

// recordSkippedViews notes filtered views in Summary.Stats (MC-9 A2) without
// touching the four-state counts shape.
func recordSkippedViews(s *Summary, skipped []string) {
	if len(skipped) > 0 {
		s.Stats["skipped_views"] = skipped
	}
}

func newSummary(c Config) *Summary {
	s := &Summary{StartedAt: time.Now(), Overall: OverallEqual,
		Counts: map[string]int{"equal": 0, "diff": 0, "error": 0, "ttl_skip": 0},
		Stats:  map[string]any{"buckets": c.Buckets, "parallel_colls": c.ParallelColls, "parallel_buckets": c.ParallelBuckets, "batch_size": c.BatchSize, "resume": c.Resume}}
	// 第9单: the summary records that a watermark window was requested —
	// the report face reads it for the 水位过滤 mode tag.
	if c.wmRequested() {
		s.Stats["watermark"] = true
	}
	return s
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

// writeSummary persists the summary.json artifact. MC-15 (adversarial MC-14
// Level-2 pair): the fixed tmp name is replaced by os.CreateTemp (no
// collisions) and write/rename failures propagate — a task whose artifact
// write failed must fail loudly instead of completing silently with a 404
// report (or a stale summary from a previous run).
func writeSummary(c Config, s *Summary) error {
	if c.OutDir == "" {
		return nil
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal summary: %w", err)
	}
	// Atomic write (temp + rename, reporter.SaveJSON pattern): a concurrent
	// report download (format=json serves the file verbatim) must never read
	// a half-written artifact (adversarial review MC-14 Level-2).
	tmp, err := os.CreateTemp(c.OutDir, "summary-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create summary temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write summary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close summary temp file: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(c.OutDir, "summary.json")); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename summary: %w", err)
	}
	return nil
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

// compareDB enumerates srcDB's non-system collections (views filtered per
// MC-9 A2) and compares each one against the same-named collection in
// dstDB. The target's name+type list is fetched ONCE per database and
// shared by every collection's schema check (MC-9 C4: kills the O(N²)
// per-collection listCollections storm). Skipped views are reported back
// for Summary.Stats (skipped_views) — they deliberately do not enter the
// four-state counts.
//
// MC-10 B1 (bidirectional existence, schema level only): dst-only
// COLLECTIONS — present on the target, absent on the source — surface as
// synthetic rows {Overall=diff, Checks.Schema=["missing-in-source"]} so a
// migration-verification run reports extra target collections too. dstNames
// counts only real collections (a same-named target VIEW no longer masks a
// missing target collection); dst-only views are not reported (symmetric
// with the source-side view filter) and system.* stays excluded on both
// sides.
func compareDB(ctx context.Context, src, dst mongoconn.Client, srcDB, dstDB string, c Config) ([]CollectionResult, []string, int, error) {
	// coll-select ①: the optional whitelist gates BOTH sides symmetrically —
	// unselected source collections are not compared and unselected target
	// collections do not surface as missing-in-source noise.
	only := map[string]bool{}
	for _, n := range c.OnlyCollections {
		only[n] = true
	}
	srcList, err := src.ListCollectionNameTypes(ctx, srcDB)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("list collections %s: %w", srcDB, err)
	}
	srcColls := make(map[string]bool, len(srcList))
	for _, ct := range srcList {
		if strings.HasPrefix(ct.Name, "system.") || !ct.IsCollection() {
			continue
		}
		if len(only) > 0 && !only[ct.Name] {
			continue
		}
		srcColls[ct.Name] = true
	}
	var dstNames map[string]bool
	var dstOnly []CollectionResult
	if c.has(LevelSchema) {
		dstList, err := dst.ListCollectionNameTypes(ctx, dstDB)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("list collections %s: %w", dstDB, err)
		}
		dstNames = make(map[string]bool, len(dstList))
		for _, ct := range dstList {
			if strings.HasPrefix(ct.Name, "system.") || !ct.IsCollection() {
				continue
			}
			if len(only) > 0 && !only[ct.Name] {
				continue
			}
			dstNames[ct.Name] = true
			if !srcColls[ct.Name] {
				var row CollectionResult
				row.DB, row.Coll, row.Overall = dstDB, ct.Name, OverallDiff
				row.Checks.Schema = map[string]any{"diff": []string{"missing-in-source"}}
				dstOnly = append(dstOnly, row)
			}
		}
	}
	type task struct{ db, coll string }
	var tasks []task
	var skippedViews []string
	for _, ct := range srcList {
		if strings.HasPrefix(ct.Name, "system.") {
			continue
		}
		// View filter (MC-9 A2): only collections carry comparable data
		// (empty type = collection, the M1 guard inside IsCollection).
		if !ct.IsCollection() {
			// An UNSELECTED view is not the user's concern — only views
			// that pass the whitelist surface in the disclosure.
			if len(only) == 0 || only[ct.Name] {
				skippedViews = append(skippedViews, srcDB+"."+ct.Name)
			}
			continue
		}
		if len(only) > 0 && !only[ct.Name] {
			continue
		}
		tasks = append(tasks, task{srcDB, ct.Name})
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
			results[i] = compareCollection(ctx, src, dst, t.db, dstDB, t.coll, dstNames, c)
		}(i, t)
	}
	wg.Wait()
	if len(dstOnly) > 0 {
		results = append(results, dstOnly...)
	}
	return results, skippedViews, len(tasks), nil
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
//
// 第9单 ⑥: WmFP stamps the watermark predicate fingerprint the checkpoint
// was produced under. Same bucket boundaries + different window ⇒ different
// content, so a fingerprint mismatch discards the checkpoint wholesale (old
// checkpoints carry "" — they stay reusable for non-watermark runs only,
// which is exactly the backward-compatible split the plan requires).
type checkpoint struct {
	AlgoVersion int              `json:"algo_version"`
	Buckets     map[int]cpBucket `json:"buckets"`
	Done        bool             `json:"done"`
	WmFP        string           `json:"wm_fp,omitempty"`
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

func loadCheckpoint(cfg Config, db, coll, wmFP string) checkpoint {
	cp := checkpoint{AlgoVersion: AlgoVersion, Buckets: map[int]cpBucket{}, WmFP: wmFP}
	if !cfg.Resume || cfg.OutDir == "" {
		return cp
	}
	b, err := os.ReadFile(cpPath(cfg.OutDir, db, coll))
	if err != nil {
		return cp
	}
	// Compare against the STORED file's own fields — unmarshaling into the
	// pre-seeded cp would let an absent wm_fp borrow the passed-in value
	// (a legacy checkpoint would then falsely match ANY fingerprint, the
	// exact vacuous-defense the fingerprint exists to prevent).
	var stored checkpoint
	if err := json.Unmarshal(b, &stored); err != nil {
		return cp
	}
	if stored.AlgoVersion != AlgoVersion {
		// Stale semantics: discard, recompute everything (no migration).
		return cp
	}
	if stored.WmFP != wmFP {
		// Same boundaries, different watermark window ⇒ different content:
		// discard, recompute (第9单 ⑥). "" vs "" is the legacy/no-watermark
		// match — old checkpoints stay reusable exactly there.
		return cp
	}
	if stored.Buckets == nil {
		stored.Buckets = map[int]cpBucket{}
	}
	return stored
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
// produce the same neutral index/count/cursor surfaces. dstNames is the
// per-db shared target listing (schema existence, MC-9 C4); nil skips the
// schema check (levels without schema).
func compareCollection(ctx context.Context, src, dst mongoconn.Client, srcDB, dstDB, coll string, dstNames map[string]bool, cfg Config) CollectionResult {
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

	// Schema: existence on target (per-db shared listing — one
	// listCollections per database, not per collection; MC-9 C4).
	if cfg.has(LevelSchema) {
		if !dstNames[coll] {
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
		// MC-9 A1: the TTL data-skip must not mask an already-detected
		// index-level diff — ttl_skip is only reachable from equal. The
		// Skipped="ttl" marker stays either way (it explains WHY the data
		// levels are absent, not the overall verdict).
		if r.Overall == OverallEqual {
			r.Overall = OverallTTLSkip
		}
		r.Checks.Skipped = "ttl"
		return r
	}

	// 第9单 ②-⑤: watermark precheck + window resolution — only when a data
	// level runs (schema/index-only configs never touch data). Plan A/P2A
	// semantics: global group precheck failure degrades THIS collection to
	// full comparison (disclosed); an override failure is a hard FAIL with
	// the 例外水位列 prefix; probe/cursor errors are ordinary error rows.
	var w *wmWindow
	if cfg.has(LevelCount) || cfg.has(LevelHash) || cfg.has(LevelDiff) {
		out := resolveWMWindow(ctx, sc, cfg, coll)
		switch {
		case out.fail != "":
			r.Overall, r.Checks.Error = OverallError, out.fail
			r.WmState = WmStateFailed
			return r
		case out.degrade != "":
			r.WmState = WmStateDegraded
			r.Note = wmFallbackNote(out.degrade)
		case out.window != nil:
			w = out.window
			r.WmState = WmStateWindowed
		}
	}
	wmFP := ""
	var wmPred bson.D
	if w != nil {
		wmFP = w.fp
		wmPred = w.pred
	}

	var srcN, dstN int64
	if cfg.has(LevelCount) {
		scN, err1 := sc.CountDocuments(ctx, wmPred)
		tcN, err2 := tc.CountDocuments(ctx, wmPred)
		if err1 != nil || err2 != nil {
			r.Overall, r.Checks.Error = OverallError, firstErr(err1, err2)
			return r
		}
		srcN, dstN = scN, tcN
		r.Checks.Count = map[string]any{"src": scN, "dst": tcN, "ok": scN == tcN}
		if scN != tcN {
			r.Overall = OverallDiff
		}
	}

	cp := loadCheckpoint(cfg, srcDB, coll, wmFP)
	if cfg.has(LevelHash) || cfg.has(LevelDiff) {
		// 第9单 ④: IDRange's $match rides the watermark predicate — the
		// bucket plan is clamped inside the window (no empty-bucket waste).
		rangeStats, err := sc.IDRange(ctx, wmPred)
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
					// 第9单 ④: every hash query $and-composes the watermark
					// predicate — first/last open buckets intersect the
					// window here (no loss, no duplication).
					srcD, err1 := HashSide(ctx, sc, wmAnd(b.Query, w), cfg.Opts, cfg.BatchSize)
					dstD, err2 := HashSide(ctx, tc, wmAnd(b.Query, w), cfg.Opts, cfg.BatchSize)
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
			rangeStats, err := sc.IDRange(ctx, wmPred)
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
			// 第9单 ④: the drill-down runs on the same windowed universe as
			// the hash level (identical $and composition).
			ds, err := DiffBucket(ctx, sc, tc, wmAnd(b.Query, w), cfg.Opts, cfg.BatchSize)
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

	// 第9单 ⑦: windowed disclosure — the note lands only on rows that
	// actually ran inside a window (degraded rows carry the fallback note
	// already); row counts join when the count level ran.
	if w != nil {
		r.Note = wmScopeNote(w, srcN, dstN, cfg.has(LevelCount))
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
