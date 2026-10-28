// compare_data_report.go renders the standalone data-comparison report
// family (MC-16) for finished /compare/{id} tasks: GET
// /api/v1/compare/tasks/{taskID}/report branches on format — html (the
// default) serves a family report built from task.json plus the
// engine-written report.json (TiMS banner + meta grid + overview/stats
// cards + the unit-11 表详情 face: 8 columns, failure-first sort, count
// pills, native <details> expand rows, thousand-separated numbers), or
// the verbatim report.json bytes (format=json). reporter.ToHTML is
// physically untouched — zero drift on the migration path (MC-16 leader
// design ruling): this file follows the MC-14 local-twin pattern instead.
//
// Read-route policy follows MC-10/MC-14: report downloads are reads — no
// token gate. Credentials and usernames never render: the meta grid shows
// host:port/database only (the task.json copy keeps the username; the
// renderer never reads that field).
package webapi

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/michaelliuyuan/timstool/internal/common/reporter"
)

// dcReportVersion is the data-compare report renderer's own version stamp
// (rendered once in the footer badge). It versions the REPORT face — the
// FE app version (web/frontend) is an independent line.
const dcReportVersion = "3.19.36"

// dcStatusCN is the data-compare report's status translator (reporter.statusCN
// twin — kept local because the reporter one is unexported; the compare engine
// reuses the reporter pass/fail/warn/skip vocabulary).
func dcStatusCN(s string) string {
	switch s {
	case "pass":
		return "通过"
	case "fail":
		return "失败"
	case "warn":
		return "警告"
	case "skip":
		return "跳过"
	default:
		return s
	}
}

// dcEndpointLabel renders one side for the meta grid: host:port/database.
// The username and password fields of the persisted configs are deliberately
// not inputs — credentials never render (MC-14 compareEndpointLabel policy).
func dcEndpointLabel(host string, port int, db string) string {
	if host != "" {
		if port != 0 {
			return fmt.Sprintf("%s:%d/%s", host, port, db)
		}
		return fmt.Sprintf("%s/%s", host, db)
	}
	return db
}

// dcSamplePercent formats a sample ratio as a clean percentage (0.01 → "1",
// 0.015 → "1.5"): the raw float product would otherwise render as
// 1.0000000000000002-style noise.
func dcSamplePercent(ratio float64) string {
	return strconv.FormatFloat(math.Round(ratio*100*100)/100, 'f', -1, 64)
}

// dcModeLabel maps the persisted mode onto the report's Chinese label. A
// watermark task's Mode IS its filter's base algorithm (#t3 create-side
// mapping); Watermark != nil therefore renders 水位过滤 · <base 中文>. The
// sample ratio derives exactly like buildCompareRunParams (<= 0 rides the
// 0.01 fallback, then the F-12 read-side clamp caps a stale persisted
// ratio above 1 at 1 — render-only, no run-side warn log) so the label
// states what the engine actually sampled.
func dcModeLabel(task *CompareTask) string {
	base := task.Mode
	if task.Watermark != nil && task.Watermark.BaseMode != "" {
		base = task.Watermark.BaseMode
	}
	var label string
	switch base {
	case "quick":
		label = "快速（行数）"
	case "sample":
		ratio := task.SampleRatio
		if ratio <= 0 {
			ratio = 0.01
		}
		if ratio > 1 {
			ratio = 1
		}
		label = fmt.Sprintf("采样（比例 %s%%）", dcSamplePercent(ratio))
	case "checksum":
		label = "校验和"
	case "":
		return "—"
	default:
		label = base
	}
	if task.Watermark != nil {
		return fmt.Sprintf("水位过滤 · %s（基础模式）", label)
	}
	return label
}

// dcComma renders n with thousand separators (99,997) — server-side number
// formatting for the 表详情 columns (unit-11 E: scan-friendly digits).
func dcComma(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	first := len(s) % 3
	if first == 0 {
		first = 3
	}
	var b strings.Builder
	b.WriteString(s[:first])
	for i := first; i < len(s); i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// dcBrief renders the 备注 cell (unit-11): the suggestion's first segment —
// the Chinese conclusion before the engine's '；' joiner or a newline — cut
// at 40 runes with an honest '…' whenever anything was cut. Display-side
// only: the engine original stays byte-identical in the expand area (C).
func dcBrief(s string) string {
	if s == "" {
		return "—"
	}
	seg := s
	if i := strings.IndexAny(seg, "；\n"); i >= 0 {
		seg = seg[:i]
	}
	r := []rune(seg)
	switch {
	case seg == s && len(r) <= 40:
		return s
	case len(r) > 40:
		return string(r[:40]) + "…"
	default:
		return seg + "…"
	}
}

// dcStatusRank orders 表详情 rows failure-first (unit-11 D): 失败 → 警告 →
// 通过/跳过. The rendered # keeps the engine's original order for
// traceability — sort is stable within a severity class.
func dcStatusRank(s reporter.Status) int {
	switch s {
	case reporter.StatusFail:
		return 0
	case reporter.StatusWarn:
		return 1
	default:
		return 2
	}
}

// dcValidatedSummaryRE matches the validator engine's canonical English
// summary ("validated N tables at level X") for the render-side Chinese
// mapping (unit-11 G).
var dcValidatedSummaryRE = regexp.MustCompile(`^validated ([0-9]+) tables at level (.+)$`)

// dcSummaryCN maps the engine's canonical English summary onto the report's
// Chinese vocabulary at the RENDER layer only — the persisted report.json
// and the format=json face stay verbatim. Anything the map cannot match
// (or matches with a blank level) passes through unchanged: the honest
// face never swallows an unknown summary.
func dcSummaryCN(s string) string {
	if m := dcValidatedSummaryRE.FindStringSubmatch(s); m != nil && strings.TrimSpace(m[2]) != "" {
		return "已校验 " + m[1] + " 张表 · 级别：" + m[2]
	}
	return s
}

// renderDataCompareReportHTML builds the standalone offline HTML report in
// the family format (reporter.ToHTML structure + the MC-14 .header block).
// All dynamic strings pass through mcHTMLEsc (the report family's escaper —
// reporter.htmlEsc twin, MC-14); numbers are formatted server-side.
func renderDataCompareReportHTML(task *CompareTask, rpt *reporter.Report) string {
	var sb strings.Builder
	sb.WriteString(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>TiMS 数据比对报告</title>
<style>
` + reporter.ReportCSS + `
.header { background: linear-gradient(135deg, #0C1222, #131B30); color: #EEF1F8; padding: 32px; border-radius: 12px; margin-bottom: 24px; border: 1px solid #263154; }
.header h1 { font-size: 28px; margin-bottom: 8px; }
.header .logo { color: #E13C3C; font-weight: 900; }
.header .subtitle { color: #97A0B5; font-size: 14px; }
.header .meta { margin-top: 16px; display: grid; grid-template-columns: repeat(auto-fit, minmax(260px, 1fr)); gap: 4px 24px; font-size: 13px; }
.header .meta div { color: #97A0B5; }
.header .meta span { color: #EEF1F8; font-weight: 500; }
th { white-space: nowrap; }
td.num { white-space: nowrap; }
.badge { white-space: nowrap; }
td:not(.num) { overflow-wrap: anywhere; }
th, td { padding: 8px 10px; }
td.num, th.colnum { text-align: right; }
thead th { position: sticky; top: 0; z-index: 5; }
tr.row-fail td { background: #FDF1F1; }
tr.row-fail td:first-child { box-shadow: inset 3px 0 0 #E13C3C; }
.rd-pill { display: inline-block; margin: 1px 3px 1px 0; padding: 1px 8px; border-radius: 999px; font-size: 12px; font-weight: 600; white-space: nowrap; }
.rd-content { background: rgba(225,60,60,0.09); color: #E13C3C; }
.rd-srconly { background: rgba(217,126,0,0.10); color: #D97E00; }
.rd-dstonly { background: #EEF0F5; color: #6B7280; }
tr.row-detail td { padding: 0 10px 8px 13px; }
tr.row-detail details { background: #F6F7FB; border: 1px solid #E6E8F0; border-radius: 8px; padding: 8px 12px; font-size: 13px; line-height: 1.7; }
tr.row-detail summary { cursor: pointer; color: #6B7280; font-size: 12px; user-select: none; }
.det-label { color: #6B7280; margin-right: 8px; }
.det-file { font-family: 'JetBrains Mono', 'Cascadia Mono', Consolas, ui-monospace, monospace; font-size: 12px; overflow-wrap: anywhere; }
.tims-version { font-size: 11px; opacity: 0.85; }
@media print {
thead th { position: static; }
tr.row-detail details > *:not(summary) { display: block !important; }
tr.row-detail details::details-content { display: block !important; content-visibility: visible !important; }
}
</style>
</head>
<body>
<div class="container">
<div class="header">
<h1><span class="logo">Ti</span>MS 数据比对报告</h1>
<div class="subtitle">`)
	sb.WriteString(mcHTMLEsc(rpt.StartTime.Format("2006-01-02 15:04:05")))
	sb.WriteString(` ~ `)
	sb.WriteString(mcHTMLEsc(rpt.EndTime.Format("2006-01-02 15:04:05")))
	sb.WriteString(`</div>
<div class="meta">`)
	fmt.Fprintf(&sb, `<div>任务 ID <span class="num">%s</span></div>`, mcHTMLEsc(task.ID))
	if task.Name != "" {
		fmt.Fprintf(&sb, `<div>任务名 <span>%s</span></div>`, mcHTMLEsc(task.Name))
	}
	fmt.Fprintf(&sb, `<div>源端 <span>%s</span></div>`, mcHTMLEsc(dcEndpointLabel(task.Source.Host, task.Source.Port, task.Source.Database)))
	fmt.Fprintf(&sb, `<div>目标 <span>%s</span></div>`, mcHTMLEsc(dcEndpointLabel(task.Target.Host, task.Target.Port, task.Target.Database)))
	fmt.Fprintf(&sb, `<div>比对模式 <span>%s</span></div>`, mcHTMLEsc(dcModeLabel(task)))
	fmt.Fprintf(&sb, `<div>耗时 <span class="num">%s</span></div>`, mcHTMLEsc(rpt.Duration))
	sb.WriteString(`</div></div>`)

	// Overview card (reporter.ToHTML 概览): status + duration + table/row
	// totals, then the summary line when the engine wrote one. statusClass
	// interpolates rpt.Status into an attribute — escaped like every other
	// dynamic string (MC-17 ①; reporter.ToHTML fixed in the same batch).
	statusClass := "overall-" + string(rpt.Status)
	sb.WriteString(`<div class="card"><h2>概览</h2><div class="stats">`)
	fmt.Fprintf(&sb, `<div class="stat"><div class="value %s">%s</div><div class="label">状态</div></div>`, mcHTMLEsc(statusClass), mcHTMLEsc(dcStatusCN(string(rpt.Status))))
	fmt.Fprintf(&sb, `<div class="stat"><div class="value">%s</div><div class="label">耗时</div></div>`, mcHTMLEsc(rpt.Duration))
	fmt.Fprintf(&sb, `<div class="stat"><div class="value">%d</div><div class="label">总表数</div></div>`, rpt.Stats.TotalTables)
	fmt.Fprintf(&sb, `<div class="stat"><div class="value">%d</div><div class="label">源端行数</div></div>`, rpt.Stats.TotalSourceRows)
	fmt.Fprintf(&sb, `<div class="stat"><div class="value">%d</div><div class="label">目标行数</div></div>`, rpt.Stats.TotalTargetRows)
	sb.WriteString(`</div>`)
	if rpt.Summary != "" {
		fmt.Fprintf(&sb, `<div class="summary">%s</div>`, mcHTMLEsc(dcSummaryCN(rpt.Summary)))
	}
	sb.WriteString(`</div>`)

	// Stats card (reporter.ToHTML 统计): the four status counts + diff rows.
	sb.WriteString(`<div class="card"><h2>统计</h2><div class="stats">`)
	fmt.Fprintf(&sb, `<div class="stat"><div class="value c-teal">%d</div><div class="label">通过</div></div>`, rpt.Stats.PassTables)
	fmt.Fprintf(&sb, `<div class="stat"><div class="value c-brand">%d</div><div class="label">失败</div></div>`, rpt.Stats.FailTables)
	fmt.Fprintf(&sb, `<div class="stat"><div class="value c-amber">%d</div><div class="label">警告</div></div>`, rpt.Stats.WarnTables)
	fmt.Fprintf(&sb, `<div class="stat"><div class="value c-muted">%d</div><div class="label">跳过</div></div>`, rpt.Stats.SkipTables)
	if rpt.Stats.TotalDiffRows != 0 {
		fmt.Fprintf(&sb, `<div class="stat"><div class="value c-brand">%d</div><div class="label">差异行数</div></div>`, rpt.Stats.TotalDiffRows)
	} else {
		sb.WriteString(`<div class="stat"><div class="value c-teal">0</div><div class="label">差异行数</div></div>`)
	}
	sb.WriteString(`</div></div>`)

	// Per-table detail (unit-11 A–F): 8 columns — # | 表名 | 状态 | 源端行数
	// | 目标行数 | 差异 | 行级差异 | 备注. 耗时 is gone (task-level duration
	// lives in the overview card), 错误/建议全文/明细文件名 moved into the
	// per-row native <details> expand area (zero JS). Rows sort
	// failure-first (失败 → 警告 → 通过/跳过) with the engine's original #
	// kept for traceability; failed rows get the light-red tint + 3px left
	// bar. Numbers render thousand-separated, right-aligned, diff>0 red.
	// The 行级差异 cell carries the three count pills (内容 n red / 仅源 n
	// orange / 仅目标 n grey) or the honest-abort note in brief; the full
	// engine sentences stay byte-identical in the expand area.
	if len(rpt.Tables) > 0 {
		sb.WriteString(`<div class="card"><h2>表详情</h2><table><thead><tr>`)
		sb.WriteString(`<th>#</th><th>表名</th><th>状态</th><th class="colnum">源端行数</th><th class="colnum">目标行数</th><th class="colnum">差异</th><th>行级差异</th><th>备注</th>`)
		sb.WriteString(`</tr></thead><tbody>`)
		order := make([]int, len(rpt.Tables))
		for i := range order {
			order[i] = i
		}
		sort.SliceStable(order, func(a, b int) bool {
			return dcStatusRank(rpt.Tables[order[a]].Status) < dcStatusRank(rpt.Tables[order[b]].Status)
		})
		for _, idx := range order {
			t := rpt.Tables[idx]
			rowClass := ""
			if t.Status == reporter.StatusFail {
				rowClass = ` class="row-fail"`
			}
			badgeClass := "badge-" + string(t.Status)
			diffHTML := `<td class="num">—</td>`
			if t.DiffRows != 0 {
				diffHTML = fmt.Sprintf(`<td class="num diff-pos">%s</td>`, mcHTMLEsc(dcComma(t.DiffRows)))
			}
			rdCell := "—"
			var det []string
			if t.RowDiffs != nil {
				rd := t.RowDiffs
				if rd.Note != "" {
					// Honest no-drill note: brief in the cell, full text in
					// the expand area.
					rdCell = mcHTMLEsc(dcBrief(rd.Note))
					det = append(det, fmt.Sprintf(`<div><span class="det-label">行级差异说明</span>%s</div>`, mcHTMLEsc(rd.Note)))
				} else {
					rdCell = fmt.Sprintf(`<span class="rd-pill rd-content">内容 %s</span><span class="rd-pill rd-srconly">仅源 %s</span><span class="rd-pill rd-dstonly">仅目标 %s</span>`,
						mcHTMLEsc(dcComma(rd.ContentDiff)), mcHTMLEsc(dcComma(rd.SrcOnly)), mcHTMLEsc(dcComma(rd.DstOnly)))
					trunc := ""
					if rd.Truncated {
						trunc = "（明细截断）"
					}
					det = append(det, fmt.Sprintf(`<div><span class="det-label">行级差异</span>内容不一致 %s · 仅源端 %s · 仅目标端 %s%s</div>`,
						mcHTMLEsc(dcComma(rd.ContentDiff)), mcHTMLEsc(dcComma(rd.SrcOnly)), mcHTMLEsc(dcComma(rd.DstOnly)), trunc))
				}
				if rd.File != "" {
					det = append(det, fmt.Sprintf(`<div><span class="det-label">明细文件</span><span class="det-file">%s</span></div>`, mcHTMLEsc(rd.File)))
				}
			}
			if t.Suggestion != "" {
				det = append(det, fmt.Sprintf(`<div><span class="det-label">建议原文</span>%s</div>`, mcHTMLEsc(t.Suggestion)))
			}
			if t.Error != "" {
				det = append(det, fmt.Sprintf(`<div><span class="det-label">错误全文</span>%s</div>`, mcHTMLEsc(t.Error)))
			}
			fmt.Fprintf(&sb,
				`<tr%s><td class="num">%d</td><td>%s</td><td><span class="badge %s">%s</span></td><td class="num">%s</td><td class="num">%s</td>%s<td>%s</td><td>%s</td></tr>`,
				rowClass, idx+1, mcHTMLEsc(t.TableName), mcHTMLEsc(badgeClass), mcHTMLEsc(dcStatusCN(string(t.Status))), mcHTMLEsc(dcComma(t.SourceRows)), mcHTMLEsc(dcComma(t.TargetRows)), diffHTML, rdCell, mcHTMLEsc(dcBrief(t.Suggestion)))
			if len(det) > 0 {
				fmt.Fprintf(&sb, `<tr class="row-detail"><td colspan="8"><details><summary>诊断详情</summary>%s</details></td></tr>`, strings.Join(det, ""))
			}
		}
		sb.WriteString(`</tbody></table></div>`)
	}

	sb.WriteString(`<div class="footer">由 TiMS (TiDB Migration Suite) 生成 &middot; 数据比对 &middot; <span class="tims-version">V`)
	sb.WriteString(mcHTMLEsc(dcReportVersion))
	sb.WriteString(`</span> &middot; `)
	sb.WriteString(mcHTMLEsc(time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString(`</div></div></body></html>`)
	return sb.String()
}
