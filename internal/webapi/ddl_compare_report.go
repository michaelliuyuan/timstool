// ddl_compare_report.go renders the DDL comparison report family for
// finished /ddl-compare/{id} runs: GET /api/v1/ddl-compare/tasks/{id}/report
// branches on format — html (default) serves the family report built from
// task.json + result.json (banner + meta grid + 六类汇总 + per-category
// detail naming every object on both sides + warnings), format=json
// streams the verbatim result.json bytes. Local twin of
// compare_data_report.go (MC-16 pattern): reporter.ToHTML stays
// physically untouched. Credentials never render (labels are
// host/schema only).
package webapi

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/michaelliuyuan/timstool/internal/common/reporter"
	"github.com/michaelliuyuan/timstool/internal/ddlcompare"
)

// dcmpReportVersion is the DDL-compare report renderer's own version
// stamp (dcReportVersion/mcReportVersion twin — rendered once in the
// footer badge).
const dcmpReportVersion = "3.20.0"

// dcmpVerdictCN translates a verdict for the report vocabulary.
func dcmpVerdictCN(v ddlcompare.Verdict) string {
	switch v {
	case ddlcompare.VerdictPass:
		return "通过"
	case ddlcompare.VerdictHint:
		return "提示"
	case ddlcompare.VerdictWarn:
		return "警告"
	case ddlcompare.VerdictFail:
		return "失败"
	default:
		return string(v)
	}
}

// dcmpModeLabel renders the normalization-mode line for the meta grid.
// v1.3: 字段级对比已取消 (A1), so loose_types is accepted-but-inert; the
// label keeps 宽松类型 for request/JSON compat with v1.2 tasks.
func dcmpModeLabel(o ddlcompare.Options) string {
	var parts []string
	if o.LooseTypes {
		parts = append(parts, "宽松类型")
	} else {
		parts = append(parts, "严格类型")
	}
	if o.IgnoreCase {
		parts = append(parts, "忽略大小写")
	} else {
		parts = append(parts, "区分大小写")
	}
	return strings.Join(parts, " · ")
}

// dcmpJoin renders a name list for a report cell (empty → "—").
func dcmpJoin(list []string) string {
	if len(list) == 0 {
		return "—"
	}
	return strings.Join(list, ", ")
}

// dcmpPK renders one side's PK (empty = 无主键).
func dcmpPK(cols []string) string {
	if len(cols) == 0 {
		return "无主键"
	}
	return strings.Join(cols, ", ")
}

// renderDDLCompareReportHTML builds the standalone offline HTML report
// (compare_data_report family: reporter.ReportCSS base + .header block).
// All dynamic strings pass through mcHTMLEsc.
func renderDDLCompareReportHTML(task *ddlCompareTask, res *ddlcompare.Result) string {
	var sb strings.Builder
	sb.WriteString(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>TiMS DDL 对比报告</title>
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
td.num { white-space: nowrap; text-align: right; }
td:not(.num) { overflow-wrap: anywhere; }
th, td { padding: 8px 10px; }
td.diff-pos { color: #E13C3C; font-weight: 600; }
tr.row-fail td { background: #FDF1F1; }
tr.row-fail td:first-child { box-shadow: inset 3px 0 0 #E13C3C; }
tr.row-warn td { background: #FDF7EE; }
tr.row-warn td:first-child { box-shadow: inset 3px 0 0 #D97E00; }
tr.row-hint td { background: #F2F5FA; }
tr.row-hint td:first-child { box-shadow: inset 3px 0 0 #5873B0; }
.badge-hint { background: rgba(88,115,176,0.10); color: #5873B0; }
.overall-hint { color: #5873B0; }
.wn { background: #FDF7EE; border: 1px solid #F0DFC2; border-radius: 8px; padding: 10px 14px; margin-bottom: 16px; font-size: 13px; line-height: 1.8; color: #8A5A00; }
.names-3 { display: grid; grid-template-columns: 1fr 1fr 1fr; gap: 12px; }
.names-3 > div { background: #F6F7FB; border: 1px solid #E6E8F0; border-radius: 8px; padding: 10px 12px; font-size: 13px; line-height: 1.9; overflow-wrap: anywhere; }
.names-3 h4 { margin: 0 0 6px 0; font-size: 12px; color: #6B7280; }
.names-3 .src { color: #E13C3C; }
.names-3 .dst { color: #D97E00; }
.names-3 .both { color: #0F9D8B; }
tr.row-detail td { padding: 0 10px 8px 13px; }
tr.row-detail details { background: #F6F7FB; border: 1px solid #E6E8F0; border-radius: 8px; padding: 8px 12px; font-size: 13px; line-height: 1.7; }
tr.row-detail summary { cursor: pointer; color: #6B7280; font-size: 12px; user-select: none; }
.tims-version { font-size: 11px; opacity: 0.85; }
@media print { .names-3 { grid-template-columns: 1fr; } }
</style>
</head>
<body>
<div class="container">
<div class="header">
<h1><span class="logo">Ti</span>MS DDL 对比报告</h1>
<div class="subtitle">`)
	sb.WriteString(mcHTMLEsc(res.GeneratedAt.Format("2006-01-02 15:04:05")))
	sb.WriteString(`</div>
<div class="meta">`)
	fmt.Fprintf(&sb, `<div>任务 ID <span>%s</span></div>`, mcHTMLEsc(task.ID))
	if task.Name != "" {
		fmt.Fprintf(&sb, `<div>任务名 <span>%s</span></div>`, mcHTMLEsc(task.Name))
	}
	fmt.Fprintf(&sb, `<div>源端 <span>%s（%s）</span></div>`, mcHTMLEsc(task.SourceLabel), mcHTMLEsc(task.SourceType))
	fmt.Fprintf(&sb, `<div>目标 <span>%s（%s）</span></div>`, mcHTMLEsc(task.TargetLabel), mcHTMLEsc(task.TargetType))
	fmt.Fprintf(&sb, `<div>对比口径 <span>%s</span></div>`, mcHTMLEsc(dcmpModeLabel(task.Options)))
	fmt.Fprintf(&sb, `<div>耗时 <span>%s</span></div>`, mcHTMLEsc(res.Duration))
	sb.WriteString(`</div></div>`)

	// Overall card + category summary (六类一览).
	sb.WriteString(`<div class="card"><h2>结论</h2><div class="stats">`)
	fmt.Fprintf(&sb, `<div class="stat"><div class="value overall-%s">%s</div><div class="label">总体结论</div></div>`,
		mcHTMLEsc(string(res.Overall)), mcHTMLEsc(dcmpVerdictCN(res.Overall)))
	for _, c := range res.Categories {
		fmt.Fprintf(&sb, `<div class="stat"><div class="value">%d / %d</div><div class="label">%s（源/目标）</div></div>`,
			c.SourceCount, c.TargetCount, mcHTMLEsc(c.Label))
	}
	if hintCats := dcmpHintCategories(res); hintCats > 0 {
		fmt.Fprintf(&sb, `<div class="stat"><div class="value" style="color:#5873B0;">%d</div><div class="label">跨方言提示类（不判错）</div></div>`, hintCats)
	}
	sb.WriteString(`</div></div>`)

	if len(res.Warnings) > 0 {
		sb.WriteString(`<div class="wn"><strong>提示</strong><br>`)
		for i, wn := range res.Warnings {
			if i > 0 {
				sb.WriteString(`<br>`)
			}
			sb.WriteString(mcHTMLEsc(wn))
		}
		sb.WriteString(`</div>`)
	}

	// Category summary table (源/目标/一致 + verdict per class).
	sb.WriteString(`<div class="card"><h2>八类汇总</h2><table><thead><tr><th>类别</th><th class="colnum">源端数</th><th class="colnum">目标端数</th><th class="colnum">一致</th><th>结论</th></tr></thead><tbody>`)
	for _, c := range res.Categories {
		fmt.Fprintf(&sb, `<tr class="row-%s"><td>%s</td><td class="num">%d</td><td class="num">%d</td><td class="num">%d</td><td><span class="badge badge-%s">%s</span></td></tr>`,
			mcHTMLEsc(string(c.Verdict)), mcHTMLEsc(c.Label), c.SourceCount, c.TargetCount, c.Matched,
			mcHTMLEsc(string(c.Verdict)), mcHTMLEsc(dcmpVerdictCN(c.Verdict)))
		if c.Note != "" {
			fmt.Fprintf(&sb, `<tr><td colspan="5"><span class="badge badge-warn">注解</span> %s</td></tr>`,
				mcHTMLEsc(c.Note))
		}
	}
	sb.WriteString(`</tbody></table></div>`)

	// Per-category detail cards.
	for _, c := range res.Categories {
		switch c.Key {
		case "tables":
			sb.WriteString(`<div class="card"><h2>表（主键 / 字符集 / 排序规则）</h2><table><thead><tr><th>#</th><th>表名</th><th>状态</th><th>主键（源 → 目标）</th><th>字符集（源 → 目标）</th><th>排序规则（源 → 目标）</th></tr></thead><tbody>`)
			for i, td := range c.Tables {
				row := "row-" + string(td.Verdict)
				pk := "—"
				if td.PKDiff != nil {
					pk = fmt.Sprintf("%s → %s", dcmpPK(td.PKDiff.Source), dcmpPK(td.PKDiff.Target))
				}
				status := dcmpTableStatus(td)
				cs := dcmpAttr(td.CharsetDiff)
				coll := dcmpAttr(td.CollationDiff)
				fmt.Fprintf(&sb, `<tr class="%s"><td class="num">%d</td><td>%s</td><td><span class="badge badge-%s">%s</span></td><td>%s</td><td>%s</td><td>%s</td></tr>`,
					mcHTMLEsc(row), i+1, mcHTMLEsc(td.Table), mcHTMLEsc(string(td.Verdict)), mcHTMLEsc(status),
					mcHTMLEsc(pk), mcHTMLEsc(cs), mcHTMLEsc(coll))
				if note := dcmpAttrNote(td.CharsetDiff, td.CollationDiff); note != "" {
					fmt.Fprintf(&sb, `<tr class="row-detail"><td colspan="6"><details><summary>V4 说明</summary>%s</details></td></tr>`, note)
				}
			}
			sb.WriteString(`</tbody></table></div>`)
		case "privileges":
			sb.WriteString(dcmpPrivilegesCard(c))
		default:
			// Name-listed classes: three columns naming every object.
			fmt.Fprintf(&sb, `<div class="card"><h2>%s（源端 %d · 目标端 %d · 两边一致 %d）</h2>`, mcHTMLEsc(c.Label), c.SourceCount, c.TargetCount, c.Matched)
			if c.Names == nil || (len(c.Names.SourceOnly) == 0 && len(c.Names.TargetOnly) == 0 && len(c.Names.Common) == 0) {
				sb.WriteString(`<div class="summary">无对象</div></div>`)
				continue
			}
			sb.WriteString(`<div class="names-3">`)
			fmt.Fprintf(&sb, `<div><h4>仅源端（%d）</h4><span class="src">%s</span></div>`, len(c.Names.SourceOnly), mcHTMLEsc(dcmpJoin(c.Names.SourceOnly)))
			fmt.Fprintf(&sb, `<div><h4>仅目标端（%d）</h4><span class="dst">%s</span></div>`, len(c.Names.TargetOnly), mcHTMLEsc(dcmpJoin(c.Names.TargetOnly)))
			fmt.Fprintf(&sb, `<div><h4>两边一致（%d）</h4><span class="both">%s</span></div>`, len(c.Names.Common), mcHTMLEsc(dcmpJoin(c.Names.Common)))
			sb.WriteString(`</div></div>`)
		}
	}

	sb.WriteString(`<div class="footer">由 TiMS (TiDB Migration Suite) 生成 &middot; DDL 对比 &middot; <span class="tims-version">V`)
	sb.WriteString(mcHTMLEsc(dcmpReportVersion))
	sb.WriteString(`</span> &middot; `)
	sb.WriteString(mcHTMLEsc(time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString(`</div></div></body></html>`)
	return sb.String()
}

// dcmpTableStatus renders the row badge text for a table row.
func dcmpTableStatus(td ddlcompare.TableDiff) string {
	switch {
	case td.SourceOnly:
		return "仅源端"
	case td.TargetOnly:
		return "仅目标端"
	default:
		return dcmpVerdictCN(td.Verdict)
	}
}

// dcmpAttr renders one attribute diff as "source → target" (equal folds
// render plainly; absent on both sides → "—").
func dcmpAttr(d *ddlcompare.AttrDiff) string {
	if d == nil {
		return "—"
	}
	src, tgt := d.Source, d.Target
	if src == "" {
		src = "(无)"
	}
	if tgt == "" {
		tgt = "(无)"
	}
	return src + " → " + tgt
}

// dcmpAttrNote collects the V4 warn reasons of a table's charset/collation
// diffs for the expand area.
func dcmpAttrNote(cs, coll *ddlcompare.AttrDiff) string {
	var lines []string
	if cs != nil && !cs.Equal && cs.Note != "" {
		lines = append(lines, "字符集："+cs.Note)
	}
	if coll != nil && !coll.Equal && coll.Note != "" {
		lines = append(lines, "排序规则："+coll.Note)
	}
	if len(lines) == 0 {
		return ""
	}
	out := ""
	for _, l := range lines {
		out += "<div>" + mcHTMLEsc(l) + "</div>"
	}
	return out
}

// dcmpPrivilegesCard renders the privileges category (V1 分轨). Same-dialect
// runs compare per-user global-privilege sets (fail-able, full lists);
// cross-dialect runs disclose both sides' raw token lists with the「词汇表
// 不相交」note and a warn badge — never a pass/fail conclusion.
func dcmpPrivilegesCard(c ddlcompare.Category) string {
	var sb strings.Builder
	sb.WriteString(`<div class="card"><h2>权限（每用户全局权限）</h2>`)
	if c.Privileges == nil {
		sb.WriteString(`<div class="summary">无数据</div></div>`)
		return sb.String()
	}
	if !c.Privileges.Comparable {
		// E2: 标注文案单一事实源 = category.note（diff.go 已写入），此处渲染
		// 仅补充解释性说明，不重复硬编码结论文案。
		note := c.Note
		if note == "" {
			note = "跨方言权限词汇表不相交，不做结论"
		}
		sb.WriteString(`<div class="wn"><strong>` + mcHTMLEsc(note) + `</strong> — MySQL 全局权限（mysql.user ` + strconv.Itoa(len(c.Privileges.Users)) + ` 类列）与 PG 布尔/角色成员词汇表无交集，以下仅按用户全量披露两侧原始权限清单。</div>`)
	}
	sb.WriteString(`<table><thead><tr><th>#</th><th>用户</th><th>源端权限</th><th>目标端权限</th><th>结论</th></tr></thead><tbody>`)
	for i, u := range c.Privileges.Users {
		badge := "badge-hint"
		text := "—"
		if c.Privileges.Comparable {
			if u.Equal {
				badge = "badge-pass"
				text = "一致"
			} else {
				badge = "badge-fail"
				if u.SourceOnly {
					text = "仅源端"
				} else if u.TargetOnly {
					text = "仅目标端"
				} else {
					text = "不一致"
				}
			}
		} else if u.Equal {
			text = "词汇表不同不判"
		} else {
			text = "词汇表不同不判"
		}
		side := "仅源端"
		if u.TargetOnly {
			side = "仅目标端"
		} else if u.SourceOnly {
			side = "仅源端"
		} else {
			side = ""
		}
		fmt.Fprintf(&sb, `<tr class="%s"><td class="num">%d</td><td>%s%s</td><td><code>%s</code></td><td><code>%s</code></td><td><span class="badge %s">%s</span></td></tr>`,
			dcmpPrivRowClass(u, c.Privileges.Comparable), i+1, mcHTMLEsc(u.User), dcmpSideTag(side),
			mcHTMLEsc(strings.Join(u.Source, ", ")), mcHTMLEsc(strings.Join(u.Target, ", ")), mcHTMLEsc(badge), mcHTMLEsc(text))
	}
	sb.WriteString(`</tbody></table></div>`)
	return sb.String()
}

// dcmpHintCategories counts hint (提示级) categories — the v1.4 cross-
// dialect downgrade surface, summarized on the 结论 card.
func dcmpHintCategories(res *ddlcompare.Result) int {
	n := 0
	for _, c := range res.Categories {
		if c.Verdict == ddlcompare.VerdictHint {
			n++
		}
	}
	return n
}

// dcmpPrivRowClass maps a privilege row to its stripe class.
func dcmpPrivRowClass(u ddlcompare.PrivUserDiff, comparable bool) string {
	if !comparable {
		return "row-hint"
	}
	if u.Equal {
		return "row-pass"
	}
	return "row-fail"
}

// dcmpSideTag renders the one-sided marker chip (empty → "").
func dcmpSideTag(side string) string {
	if side == "" {
		return ""
	}
	return ` <span class="badge badge-warn">` + side + `</span>`
}
