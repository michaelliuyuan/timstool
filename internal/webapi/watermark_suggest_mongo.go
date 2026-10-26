// watermark_suggest_mongo.go is the MongoDB watermark candidate catalog
// (mongo 水位列自动发现单, thread seq=39 批准方案①②): MongoDB has no schema
// catalog, so one bounded pass per collection builds the SAME row shape the
// pg/mysql suggest flow scores —
//
//	wmCatalogColumn{Table, Column, DataType, Indexed, DefaultNow}
//
// from two signals only, both read-only and bounded:
//
//   - index prefixes: IndexSpecs() is metadata-only; a field is Indexed when
//     it is the FIRST key of an index with a numeric direction (1/-1 — a
//     range predicate {field:{$gte,$lt}} prefix-scans such an index; text/
//     hashed/geo keys never do).
//   - doc sampling: Find({}) with a hard 50-doc limit builds a per-field
//     BSON type histogram over the engine's custom-column whitelist
//     (DateTime→datetime, int64→bigint, int32→integer — the same set
//     mongocompare.wmConvertibleType admits); the most frequent comparable
//     type is the representative, ties prefer the time semantics.
//
// An index-prefix field the sample never showed gets ONE $exists probe
// (Batch/Limit 1, the engine's own probe shape) before being dropped — the
// "utime exists but sits beyond the first 50 docs" case. Fields the sample
// DID show carry only non-comparable types (string/array/…) are dropped
// without a probe: the sample already bears type evidence.
//
// The rows then flow VERBATIM through the shared pure scorer
// (scoreWatermarkCandidates — zero modifications, red line): coverage /
// name-class / type / index weights, top-5, Chinese reasons/warnings. Mongo
// has no DEFAULT now() analog, so default_now_ratio is structurally 0 for
// every candidate — the shared 10% factor scales all mongo candidates
// equally and never reorders them. ObjectId fields map to no scorer type
// (engine whitelist keeps them _id-only); _id therefore surfaces here only
// when it carries int/long, and the FE keeps its own _id default option.
//
// Concurrency: 4 collections in flight (the engine's semaphore pattern),
// 60s budget, first error fails loud (advisory endpoint — a half-scanned
// catalog must not rank anything). Views and system.* collections never
// participate.
package webapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/michaelliuyuan/timstool/internal/mongoconn"
	"github.com/michaelliuyuan/timstool/internal/source"
)

const (
	wmMongoSampleDocs    = 50               // hard per-collection sample limit
	wmMongoSuggestPar    = 4                // collections in flight
	wmMongoSuggestBudget = 60 * time.Second // whole-catalog budget (pg suggest parity)
)

// wmMongoDataType maps one BSON value onto the scorer's type labels. The
// membership is the engine's custom-column whitelist verbatim (DateTime /
// int32 / int64); ObjectId/double stay _id-only and are not suggestible.
func wmMongoDataType(v any) (dataType string, ok bool) {
	switch v.(type) {
	case bson.DateTime:
		return "datetime", true
	case int64:
		return "bigint", true
	case int32:
		return "integer", true
	}
	return "", false
}

// wmMongoRepType picks a field's representative label from its comparable
// histogram: most occurrences wins; ties resolve toward the time semantics
// (datetime > bigint > integer) — a watermark column's whole point is
// monotonic time advance.
func wmMongoRepType(counts map[string]int) string {
	best, bestN := "", 0
	for _, dt := range []string{"datetime", "bigint", "integer"} {
		if counts[dt] > bestN {
			best, bestN = dt, counts[dt]
		}
	}
	return best
}

// wmMongoIndexedFields returns the fields that can prefix-serve a range
// predicate: first key of any index with a numeric direction. mongoconn
// folds ±1.0/int64/int32 orderings onto int32 at decode (normalizeIndexOrder),
// so the numeric switch covers every range-capable spelling; string orders
// ("text", "hashed", "2d", …) never prefix-scan.
//
// 对抗席 a1/a2: keys failing the shared identifier allow-list
// (incIdentifierOK — the SAME gate normalizeWatermarkFilter runs on the
// watermark column at task creation) never enter the set: the "$**"
// wildcard-index key would otherwise survive to a {"$**": {$exists}} probe
// that the server rejects as an unknown operator (one wildcard index would
// 500 the whole catalog), and non-ASCII/dotted keys would surface as
// candidates task creation must then reject ("suggests what it refuses").
func wmMongoIndexedFields(specs []source.IndexInfo) map[string]bool {
	out := map[string]bool{}
	for _, ix := range specs {
		if len(ix.Keys) == 0 {
			continue
		}
		k := ix.Keys[0]
		if !incIdentifierOK(k.Field) {
			continue
		}
		switch k.Order.(type) {
		case int32, int64, float64:
			out[k.Field] = true
		}
	}
	return out
}

// wmMongoFieldOf extracts a top-level field from a probe document (the
// engine's wmFieldOf shape — column names come from index keys / sampled
// docs, never user input, so no dotted-path traversal).
func wmMongoFieldOf(doc bson.D, col string) (any, bool) {
	for _, e := range doc {
		if e.Key == col {
			return e.Value, true
		}
	}
	return nil, false
}

// wmMongoCollCatalog builds one collection's catalog rows (sample histogram
// ∪ index-prefix fields, probe-back for the sample-unseen). Deterministic
// order: sorted by column name.
func wmMongoCollCatalog(ctx context.Context, c mongoconn.Coll) ([]wmCatalogColumn, error) {
	specs, err := c.IndexSpecs(ctx)
	if err != nil {
		return nil, err
	}
	indexed := wmMongoIndexedFields(specs)

	it, err := c.Find(ctx, bson.D{}, mongoconn.FindOpts{Batch: wmMongoSampleDocs, Limit: wmMongoSampleDocs})
	if err != nil {
		return nil, err
	}
	type fieldAcc struct {
		counts map[string]int
	}
	hist := map[string]*fieldAcc{}
	seen := map[string]bool{}
	for it.Next(ctx) {
		for _, e := range it.Doc() {
			seen[e.Key] = true
			// 对抗席 a2: sampled field names face the creation-time
			// identifier gate here — a field task creation would reject
			// (non-ASCII, dotted, $-prefixed) never becomes a candidate.
			if !incIdentifierOK(e.Key) {
				continue
			}
			if dt, ok := wmMongoDataType(e.Value); ok {
				a := hist[e.Key]
				if a == nil {
					a = &fieldAcc{counts: map[string]int{}}
					hist[e.Key] = a
				}
				a.counts[dt]++
			}
		}
	}
	if err := it.Err(); err != nil {
		_ = it.Close(ctx)
		return nil, err
	}
	if err := it.Close(ctx); err != nil {
		return nil, err
	}

	var rows []wmCatalogColumn
	for field, a := range hist {
		rows = append(rows, wmCatalogColumn{
			Table: "", Column: field, // Table stamped by the caller
			DataType: wmMongoRepType(a.counts),
			Indexed:  indexed[field],
		})
	}
	for field := range indexed {
		if _, ok := hist[field]; ok {
			continue // already scored from sample evidence
		}
		if seen[field] {
			continue // sample showed it with only non-comparable types
		}
		// Sample-unseen index field: ONE $exists probe (engine shape).
		probeQ := bson.D{{Key: field, Value: bson.D{{Key: "$exists", Value: true}}}}
		pit, perr := c.Find(ctx, probeQ, mongoconn.FindOpts{Batch: 1, Limit: 1})
		if perr != nil {
			return nil, perr
		}
		var pd bson.D
		if pit.Next(ctx) {
			pd = pit.Doc()
		}
		pitErr := pit.Err()
		_ = pit.Close(ctx)
		if pitErr != nil {
			return nil, pitErr
		}
		if v, ok := wmMongoFieldOf(pd, field); ok {
			if dt, ok2 := wmMongoDataType(v); ok2 {
				rows = append(rows, wmCatalogColumn{Column: field, DataType: dt, Indexed: true})
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Column < rows[j].Column })
	return rows, nil
}

// wmMongoCatalog runs the per-collection pass over a database's user
// collections (views and system.* excluded), 4 in flight, fail-loud on the
// first error. Row order is deterministic (Table, Column).
func wmMongoCatalog(ctx context.Context, client mongoconn.Client, db string) ([]wmCatalogColumn, error) {
	cts, err := client.ListCollectionNameTypes(ctx, db)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, ct := range cts {
		if !ct.IsCollection() {
			continue
		}
		if strings.HasPrefix(ct.Name, "system.") {
			continue
		}
		names = append(names, ct.Name)
	}
	if len(names) == 0 {
		return nil, nil
	}

	sem := make(chan struct{}, wmMongoSuggestPar)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var rows []wmCatalogColumn
	var firstErr error
	for _, name := range names {
		wg.Add(1)
		sem <- struct{}{}
		go func(name string) {
			defer wg.Done()
			defer func() { <-sem }()
			r, cerr := wmMongoCollCatalog(ctx, client.Collection(db, name))
			mu.Lock()
			defer mu.Unlock()
			if cerr != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("%s: %w", name, cerr)
				}
				return
			}
			for i := range r {
				r[i].Table = name
			}
			rows = append(rows, r...)
		}(name)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Table != rows[j].Table {
			return rows[i].Table < rows[j].Table
		}
		return rows[i].Column < rows[j].Column
	})
	return rows, nil
}

// handleSuggestMongoWatermark: POST /mongocompare/suggest-watermark
// {source} — source is the task-create endpoint shape (datasource_id or
// discrete fields; database REQUIRED and is the catalog target). Response is
// the pg suggest shape verbatim: {total_tables, candidates}. Advisory only:
// no task, no store write; the run-time $exists probe stays the authority
// (a mismatched candidate degrades/fails per the engine's disclosed
// semantics, never silently).
func (s *Server) handleSuggestMongoWatermark(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source compareEndpoint `json:"source"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := s.resolveCompareRef(&req.Source); err != nil {
		s.writeError(w, http.StatusBadRequest, "source: "+err.Error())
		return
	}
	dc, err := req.Source.dialConfig()
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "source: "+err.Error())
		return
	}
	if req.Source.Database == "" {
		s.writeError(w, http.StatusBadRequest, "source.database is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), wmMongoSuggestBudget)
	defer cancel()
	client, err := mongoconn.Dial(ctx, dc)
	if err != nil {
		s.writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer dcancel()
		_ = client.Disconnect(dctx)
	}()

	cols, err := wmMongoCatalog(ctx, client, req.Source.Database)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "catalog query failed: "+err.Error())
		return
	}
	total, cands := scoreWatermarkCandidates(cols)
	if cands == nil {
		cands = []wmSuggestCandidate{}
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_tables": total,
		"candidates":   cands,
	})
}
