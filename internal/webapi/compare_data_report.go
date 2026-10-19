// compare_data_report.go renders the standalone data-comparison report
// family (MC-16) for finished /compare/{id} tasks: GET
// /api/v1/compare/tasks/{taskID}/report branches on format — html (the
// default) serves a family report built from task.json plus the
// engine-written report.json (TiMS banner + meta grid + overview/stats
// cards + per-table detail keeping the compare-specific 建议 column), or
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
	"strconv"
	"strings"
	"time"

	"github.com/michaelliuyuan/timstool/internal/common/reporter"
)

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
// 0.01 fallback) so the label states what the engine actually sampled.
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
	// totals, then the summary line when the engine wrote one.
	statusClass := "overall-" + string(rpt.Status)
	sb.WriteString(`<div class="card"><h2>概览</h2><div class="stats">`)
	fmt.Fprintf(&sb, `<div class="stat"><div class="value %s">%s</div><div class="label">状态</div></div>`, statusClass, mcHTMLEsc(dcStatusCN(string(rpt.Status))))
	fmt.Fprintf(&sb, `<div class="stat"><div class="value">%s</div><div class="label">耗时</div></div>`, mcHTMLEsc(rpt.Duration))
	fmt.Fprintf(&sb, `<div class="stat"><div class="value">%d</div><div class="label">总表数</div></div>`, rpt.Stats.TotalTables)
	fmt.Fprintf(&sb, `<div class="stat"><div class="value">%d</div><div class="label">源端行数</div></div>`, rpt.Stats.TotalSourceRows)
	fmt.Fprintf(&sb, `<div class="stat"><div class="value">%d</div><div class="label">目标行数</div></div>`, rpt.Stats.TotalTargetRows)
	sb.WriteString(`</div>`)
	if rpt.Summary != "" {
		fmt.Fprintf(&sb, `<div class="summary">%s</div>`, mcHTMLEsc(rpt.Summary))
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

	// Per-table detail: the migration-report column set plus the
	// compare-specific 建议 column (D3 keeps it — it is the actionable
	// signal for a data compare).
	if len(rpt.Tables) > 0 {
		sb.WriteString(`<div class="card"><h2>表详情</h2><table><thead><tr>`)
		sb.WriteString(`<th>#</th><th>表名</th><th>状态</th><th>源端行数</th><th>目标行数</th><th>差异</th><th>耗时</th><th>错误</th><th>建议</th>`)
		sb.WriteString(`</tr></thead><tbody>`)
		for i, t := range rpt.Tables {
			badgeClass := "badge-" + string(t.Status)
			diffStr := ""
			if t.DiffRows != 0 {
				diffStr = fmt.Sprintf("%d", t.DiffRows)
			}
			// Honest "—" for a table with no measurable duration instead of
			// a silently blank cell (reporter.ToHTML precedent).
			durStr := t.Duration
			if durStr == "" {
				durStr = "—"
			}
			fmt.Fprintf(&sb,
				`<tr><td class="num">%d</td><td>%s</td><td><span class="badge %s">%s</span></td><td class="num">%d</td><td class="num">%d</td><td class="num">%s</td><td class="num">%s</td><td>%s</td><td>%s</td></tr>`,
				i+1, mcHTMLEsc(t.TableName), mcHTMLEsc(badgeClass), mcHTMLEsc(dcStatusCN(string(t.Status))), t.SourceRows, t.TargetRows, diffStr, mcHTMLEsc(durStr), mcHTMLEsc(t.Error), mcHTMLEsc(t.Suggestion))
		}
		sb.WriteString(`</tbody></table></div>`)
	}

	sb.WriteString(`<div class="footer">由 TiMS (TiDB Migration Suite) 生成 &middot; 数据比对 &middot; `)
	sb.WriteString(mcHTMLEsc(time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString(`</div></div></body></html>`)
	return sb.String()
}
