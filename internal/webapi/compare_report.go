// compare_report.go renders the MongoDB compare report family (MC-14) for
// finished mongocompare tasks: GET /api/v1/tasks/{taskID}/report branches on
// the ConfigJSON kind — a compare task serves a detailed HTML report built
// from the engine-written summary.json (banner + four-state counts +
// per-collection five-level detail + mismatched-bucket drill-down), or the
// verbatim summary.json bytes (format=json). Migration tasks never parse as
// compare configs and keep the legacy report path untouched.
//
// Read-route policy follows MC-10: report downloads are reads — no token
// gate, same as the task GETs. Credentials never render: the header shows
// host:port/database only (URI-mode endpoints render as "URI 连接" without
// the URI string, which may embed credentials).
package webapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/michaelliuyuan/timstool/internal/common/reporter"
	"github.com/michaelliuyuan/timstool/internal/mongocompare"
	"github.com/michaelliuyuan/timstool/internal/store"
)

// loadCompareSummary reads and decodes the engine-written summary.json.
func loadCompareSummary(path string) (*mongocompare.Summary, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sum mongocompare.Summary
	if err := json.Unmarshal(b, &sum); err != nil {
		return nil, fmt.Errorf("decode summary.json: %w", err)
	}
	return &sum, nil
}

// handleCompareTaskReport is the mongocompare branch of handleTaskReport.
// format=json serves the verbatim summary.json bytes (byte-identical to the
// engine artifact); html (default) renders the detailed report. A missing
// summary.json (never-run, connect-failed, wiped rerun, or a failed artifact
// write — MC-15: engine write failures now fail the task) is a 404 with a
// message that names the cause — never a half-empty report. An UNREADABLE
// summary (corrupt artifact) is a 500 — the file exists, so "not produced"
// would misattribute the cause (adversarial review MC-14 Level-2).
func (s *Server) handleCompareTaskReport(w http.ResponseWriter, r *http.Request, task *store.Task, spec *compareTaskSpec) {
	summaryPath := filepath.Join(s.dataDir, "compare", task.ID, "summary.json")
	if r.URL.Query().Get("format") == "json" {
		b, err := os.ReadFile(summaryPath)
		if err != nil {
			s.writeCompareSummaryError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=compare-summary-%s.json", task.ID))
		w.Write(b)
		return
	}
	sum, err := loadCompareSummary(summaryPath)
	if err != nil {
		s.writeCompareSummaryError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=compare-report-%s.html", task.ID))
	w.Write([]byte(renderCompareReportHTML(task, spec, sum)))
}

// writeCompareSummaryError maps missing → 404 (cause named — MC-15: the
// closed cause set now includes the write-failure branch, matching the
// engine's fail-loud writeSummary) and every other read/decode failure → 500
// (the artifact exists but is unreadable).
func (s *Server) writeCompareSummaryError(w http.ResponseWriter, err error) {
	if os.IsNotExist(err) {
		s.writeError(w, http.StatusNotFound, "compare summary not found — the task has not produced summary.json (unfinished, failed before compare, wiped by a rerun, or the summary write failed — see the task error)")
		return
	}
	s.writeError(w, http.StatusInternalServerError, "read compare summary: "+err.Error())
}

// compareOverallMeta maps the engine's four overall states onto the report
// family's badge/color vocabulary (equal borrows pass, diff borrows warn so
// a data mismatch never reads as red as a hard error).
func compareOverallMeta(overall string) (badge, cn, overallClass string) {
	switch overall {
	case mongocompare.OverallEqual:
		return "badge-pass", "一致", "overall-pass"
	case mongocompare.OverallDiff:
		return "badge-warn", "存在差异", "overall-warn"
	case mongocompare.OverallError:
		return "badge-fail", "对比出错", "overall-fail"
	case mongocompare.OverallTTLSkip:
		return "badge-skip", "TTL 跳过", "c-muted"
	default:
		return "badge-skip", overall, "c-muted"
	}
}

// compareEndpointLabel renders one side for the banner header: host:port/db,
// falling back to a credential-free "URI 连接" marker for URI-mode endpoints
// (the URI string itself may embed user:password and must not render).
func compareEndpointLabel(e compareEndpoint) string {
	db := e.Database
	if e.Host != "" {
		if e.Port != 0 {
			return fmt.Sprintf("%s:%d/%s", e.Host, e.Port, db)
		}
		return fmt.Sprintf("%s/%s", e.Host, db)
	}
	if e.URI != "" {
		return fmt.Sprintf("URI 连接/%s", db)
	}
	return db
}

// hashBucketStats summarizes a collection's bucket results: matched vs total.
func hashBucketStats(buckets []mongocompare.BucketResult) (matched, total int) {
	for _, b := range buckets {
		if b.Match {
			matched++
		}
	}
	return matched, len(buckets)
}

// mcHTMLEsc is the report family's escaper (reporter.htmlEsc twin — kept
// local because the reporter one is unexported).
func mcHTMLEsc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// diffListValues normalizes a checks diff list from either []string (native
// engine values) or []any (json-decoded summary from disk) into strings.
func diffListValues(v any) []string {
	switch d := v.(type) {
	case []string:
		return d
	case []any:
		out := make([]string, 0, len(d))
		for _, x := range d {
			out = append(out, fmt.Sprint(x))
		}
		return out
	}
	return nil
}

// schemaCheckLabel renders the schema (existence) check cell.
func schemaCheckLabel(m map[string]any) string {
	if m == nil {
		return "—"
	}
	if ok, _ := m["ok"].(bool); ok {
		return "✓"
	}
	if d := diffListValues(m["diff"]); len(d) > 0 {
		labels := make([]string, 0, len(d))
		for _, v := range d {
			switch v {
			case "missing-in-target":
				labels = append(labels, "目标端缺失")
			case "missing-in-source":
				labels = append(labels, "源端缺失")
			default:
				labels = append(labels, v)
			}
		}
		return strings.Join(labels, "、")
	}
	return "?"
}

// indexCheckLabel renders the index check cell.
func indexCheckLabel(m map[string]any) string {
	if m == nil {
		return "—"
	}
	if ok, _ := m["ok"].(bool); ok {
		return "✓"
	}
	if d := diffListValues(m["diff"]); len(d) > 0 {
		return fmt.Sprintf("差异 %d 项", len(d))
	}
	return "?"
}

// countCheckLabel renders the count check cell as "src → dst".
func countCheckLabel(m map[string]any) string {
	if m == nil {
		return "—"
	}
	src, _ := m["src"].(float64)
	dst, _ := m["dst"].(float64)
	if ok, _ := m["ok"].(bool); ok {
		return fmt.Sprintf("%d → %d ✓", int64(src), int64(dst))
	}
	return fmt.Sprintf("%d → %d", int64(src), int64(dst))
}

// renderCompareReportHTML builds the standalone offline HTML report. All
// dynamic strings pass through mcHTMLEsc; numbers are formatted server-side.
// Mismatched-bucket drill-down lists at most 20 buckets per collection (an
// adversarial 4096-bucket run must not produce a megabyte table row).
func renderCompareReportHTML(task *store.Task, spec *compareTaskSpec, sum *mongocompare.Summary) string {
	_, cn, overallClass := compareOverallMeta(sum.Overall)

	var sb strings.Builder
	sb.WriteString(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>TiMS MongoDB 对比报告</title>
<style>
` + reporter.ReportCSS + `
.header { background: linear-gradient(135deg, #0C1222, #131B30); color: #EEF1F8; padding: 32px; border-radius: 12px; margin-bottom: 24px; border: 1px solid #263154; }
.header h1 { font-size: 28px; margin-bottom: 8px; }
.header .logo { color: #E13C3C; font-weight: 900; }
.header .subtitle { color: #97A0B5; font-size: 14px; }
.header .meta { margin-top: 16px; display: grid; grid-template-columns: repeat(auto-fit, minmax(260px, 1fr)); gap: 4px 24px; font-size: 13px; }
.header .meta div { color: #97A0B5; }
.header .meta span { color: #EEF1F8; font-weight: 500; }
.bucket-list { margin: 8px 0 0 24px; padding: 8px 16px; border-left: 3px solid #E6E8F0; color: #2A3040; font-size: 13px; }
.bucket-list .num { font-size: 13px; }
.coll-sub { padding: 14px 12px; border-bottom: 1px solid #E6E8F0; background: #FAFBFD; }
.coll-sub:last-child { border-bottom: none; }
.coll-sub h3 { font-size: 14px; margin-bottom: 6px; color: #0C1222; }
</style>
</head>
<body>
<div class="container">
<div class="header">
<h1><span class="logo">Ti</span>MS MongoDB 对比报告</h1>
<div class="subtitle">`)
	sb.WriteString(mcHTMLEsc(sum.StartedAt.Format("2006-01-02 15:04:05")))
	sb.WriteString(` ~ `)
	sb.WriteString(mcHTMLEsc(sum.FinishedAt.Format("2006-01-02 15:04:05")))
	sb.WriteString(`</div>
<div class="meta">`)
	fmt.Fprintf(&sb, `<div>任务 ID <span class="num">%s</span></div>`, mcHTMLEsc(task.ID))
	if task.Name != "" {
		fmt.Fprintf(&sb, `<div>任务名 <span>%s</span></div>`, mcHTMLEsc(task.Name))
	}
	if spec != nil {
		fmt.Fprintf(&sb, `<div>源端 <span>%s</span></div>`, mcHTMLEsc(compareEndpointLabel(spec.Source)))
		fmt.Fprintf(&sb, `<div>目标端 <span>%s</span></div>`, mcHTMLEsc(compareEndpointLabel(spec.Target)))
	}
	fmt.Fprintf(&sb, `<div>耗时 <span class="num">%.2fs</span></div>`, sum.DurationSec)
	sb.WriteString(`</div></div>`)

	// Overview stats: four-state counts (the engine's own vocabulary).
	sb.WriteString(`<div class="card"><h2>汇总</h2><div class="stats">`)
	fmt.Fprintf(&sb, `<div class="stat"><div class="value %s">%s</div><div class="label">总体结果</div></div>`, overallClass, mcHTMLEsc(cn))
	fmt.Fprintf(&sb, `<div class="stat"><div class="value">%d</div><div class="label">集合总数</div></div>`, len(sum.Collections))
	fmt.Fprintf(&sb, `<div class="stat"><div class="value c-teal">%d</div><div class="label">一致</div></div>`, sum.Counts["equal"])
	fmt.Fprintf(&sb, `<div class="stat"><div class="value c-amber">%d</div><div class="label">差异</div></div>`, sum.Counts["diff"])
	fmt.Fprintf(&sb, `<div class="stat"><div class="value c-brand">%d</div><div class="label">出错</div></div>`, sum.Counts["error"])
	fmt.Fprintf(&sb, `<div class="stat"><div class="value c-muted">%d</div><div class="label">TTL 跳过</div></div>`, sum.Counts["ttl_skip"])
	sb.WriteString(`</div>`)

	// Engine knobs actually in effect (withDefaults applied by the engine —
	// zeros here mean the task rode the defaults and summary recorded them).
	if len(sum.Stats) > 0 {
		sb.WriteString(`<div class="summary num">引擎参数：`)
		var parts []string
		for _, k := range []string{"buckets", "parallel_colls", "parallel_buckets", "batch_size"} {
			if v, ok := sum.Stats[k]; ok {
				parts = append(parts, fmt.Sprintf("%s=%v", k, v))
			}
		}
		if v, ok := sum.Stats["resume"].(bool); ok && v {
			parts = append(parts, "resume=true")
		}
		if views, ok := sum.Stats["skipped_views"]; ok && fmt.Sprint(views) != "[]" {
			parts = append(parts, fmt.Sprintf("skipped_views=%v", views))
		}
		sb.WriteString(mcHTMLEsc(strings.Join(parts, " · ")))
		sb.WriteString(`</div>`)
	}
	sb.WriteString(`</div>`)

	// Per-collection detail table.
	if len(sum.Collections) > 0 {
		sb.WriteString(`<div class="card"><h2>逐集合明细</h2><table><thead><tr>`)
		sb.WriteString(`<th>#</th><th>集合</th><th>状态</th><th>存在性</th><th>索引</th><th>文档数 (源→目标)</th><th>哈希桶 (匹配/总数)</th><th>下钻差异</th><th>错误</th>`)
		sb.WriteString(`</tr></thead><tbody>`)
		for i, c := range sum.Collections {
			cb, ccn, _ := compareOverallMeta(c.Overall)
			matched, total := hashBucketStats(c.Checks.Hash)
			hashCell := "—"
			if c.Checks.Hash != nil {
				hashCell = fmt.Sprintf("%d/%d", matched, total)
			}
			diffCell := "—"
			if c.Checks.Diff != nil {
				if n, ok := c.Checks.Diff["count"].(float64); ok {
					diffCell = fmt.Sprintf("%d", int64(n))
				}
			}
			ttlNote := ""
			if c.Checks.Skipped == "ttl" {
				ttlNote = ` <span class="badge badge-skip">TTL 数据跳过</span>`
			}
			fmt.Fprintf(&sb,
				`<tr><td class="num">%d</td><td>%s</td><td><span class="badge %s">%s</span>%s</td><td>%s</td><td>%s</td><td class="num">%s</td><td class="num">%s</td><td class="num">%s</td><td>%s</td></tr>`,
				i+1, mcHTMLEsc(c.DB+"."+c.Coll), mcHTMLEsc(cb), mcHTMLEsc(ccn), ttlNote,
				mcHTMLEsc(schemaCheckLabel(c.Checks.Schema)), mcHTMLEsc(indexCheckLabel(c.Checks.Index)),
				mcHTMLEsc(countCheckLabel(c.Checks.Count)), mcHTMLEsc(hashCell), mcHTMLEsc(diffCell),
				mcHTMLEsc(c.Checks.Error))
		}
		sb.WriteString(`</tbody></table></div>`)
	}

	// Drill-down: mismatched buckets per non-equal collection (capped at 20
	// buckets each — the counts are the actionable signal; full ndjson
	// artifacts stay in the task's diff/ directory server-side).
	var drilldowns int
	for _, c := range sum.Collections {
		if c.Overall == mongocompare.OverallEqual {
			continue
		}
		var mism []mongocompare.BucketResult
		for _, b := range c.Checks.Hash {
			if !b.Match {
				mism = append(mism, b)
			}
		}
		diffN := int64(-1)
		if c.Checks.Diff != nil {
			if n, ok := c.Checks.Diff["count"].(float64); ok {
				diffN = int64(n)
			}
		}
		if len(mism) == 0 && diffN <= 0 && c.Checks.Error == "" && c.Checks.Schema == nil && c.Checks.Index == nil {
			continue
		}
		if drilldowns == 0 {
			sb.WriteString(`<div class="card"><h2>差异下钻（失配桶）</h2>`)
		}
		drilldowns++
		fmt.Fprintf(&sb, `<div class="coll-sub"><h3>%s</h3>`, mcHTMLEsc(c.DB+"."+c.Coll))
		if c.Checks.Error != "" {
			fmt.Fprintf(&sb, `<div class="c-brand">错误：%s</div>`, mcHTMLEsc(c.Checks.Error))
		}
		if diffN > 0 {
			fmt.Fprintf(&sb, `<div>字段级差异文档 <span class="num c-brand">%d</span> 条（明细见任务 diff 目录 ndjson）</div>`, diffN)
		} else if diffN == 0 {
			sb.WriteString(`<div>字段级差异文档 <span class="num c-teal">0</span> 条</div>`)
		}
		if len(mism) > 0 {
			sb.WriteString(`<div class="bucket-list">`)
			limit := len(mism)
			if limit > 20 {
				limit = 20
			}
			for _, b := range mism[:limit] {
				fmt.Fprintf(&sb, `<div>桶 <span class="num">#%d</span>：源 <span class="num">%d</span> / 目标 <span class="num">%d</span></div>`, b.Bucket, b.SrcCount, b.DstCount)
			}
			if len(mism) > 20 {
				fmt.Fprintf(&sb, `<div>… 共 %d 个失配桶（仅列前 20）</div>`, len(mism))
			}
			sb.WriteString(`</div>`)
		}
		sb.WriteString(`</div>`)
	}
	if drilldowns > 0 {
		sb.WriteString(`</div>`)
	}

	sb.WriteString(`<div class="footer">由 TiMS (TiDB Migration Suite) 生成 · MongoDB 一致性对比 · `)
	sb.WriteString(mcHTMLEsc(time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString(`</div></div></body></html>`)
	return sb.String()
}
