package reporter

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func FormatDuration(d time.Duration) string {
	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	seconds := math.Mod(d.Seconds(), 60)
	if hours > 0 {
		return fmt.Sprintf("%dh%dm%.3fs", hours, minutes, seconds)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm%.3fs", minutes, seconds)
	}
	return fmt.Sprintf("%.3fs", seconds)
}

type Status string

const (
	StatusPass Status = "pass"
	StatusFail Status = "fail"
	StatusWarn Status = "warn"
	StatusSkip Status = "skip"
)

type TableReport struct {
	TableName  string `json:"table_name"`
	Status     Status `json:"status"`
	Duration   string `json:"duration,omitempty"`
	SourceRows int64  `json:"source_rows,omitempty"`
	TargetRows int64  `json:"target_rows,omitempty"`
	DiffRows   int64  `json:"diff_rows,omitempty"`
	Error      string `json:"error,omitempty"`
	Suggestion string `json:"suggestion,omitempty"`
}

type Report struct {
	Tool      string        `json:"tool"`
	Version   string        `json:"version"`
	Phase     string        `json:"phase"`
	StartTime time.Time     `json:"start_time"`
	EndTime   time.Time     `json:"end_time"`
	Duration  string        `json:"duration"`
	Status    Status        `json:"overall_status"`
	Tables    []TableReport `json:"tables"`
	Summary   string        `json:"summary,omitempty"`
	Stats     ReportStats   `json:"stats"`

	// Optional header metadata for the HTML report (task identity,
	// source → target, migration mode). Empty values are not rendered.
	TaskID string `json:"task_id,omitempty"`
	Source string `json:"source,omitempty"`
	Target string `json:"target,omitempty"`
	Mode   string `json:"mode,omitempty"`
}

type ReportStats struct {
	TotalTables     int   `json:"total_tables"`
	PassTables      int   `json:"pass_tables"`
	FailTables      int   `json:"fail_tables"`
	WarnTables      int   `json:"warn_tables"`
	SkipTables      int   `json:"skip_tables"`
	TotalSourceRows int64 `json:"total_source_rows"`
	TotalTargetRows int64 `json:"total_target_rows"`
	TotalDiffRows   int64 `json:"total_diff_rows"`
}

func NewReport(phase string) *Report {
	return &Report{
		Tool:      "timstool-migrator",
		Version:   "0.1.0",
		Phase:     phase,
		StartTime: time.Now(),
		Tables:    []TableReport{},
	}
}

func (r *Report) AddTableReport(tr TableReport) {
	r.Tables = append(r.Tables, tr)
}

func (r *Report) Finish(status Status, summary string) {
	r.EndTime = time.Now()
	r.Duration = FormatDuration(r.EndTime.Sub(r.StartTime))
	r.Status = status
	r.Summary = summary
	r.computeStats()
}

func (r *Report) computeStats() {
	r.Stats = ReportStats{
		TotalTables: len(r.Tables),
	}
	for _, t := range r.Tables {
		r.Stats.TotalSourceRows += t.SourceRows
		r.Stats.TotalTargetRows += t.TargetRows
		r.Stats.TotalDiffRows += t.DiffRows
		switch t.Status {
		case StatusPass:
			r.Stats.PassTables++
		case StatusFail:
			r.Stats.FailTables++
		case StatusWarn:
			r.Stats.WarnTables++
		case StatusSkip:
			r.Stats.SkipTables++
		}
	}
}

func (r *Report) OverallStatus() Status {
	failCount := 0
	warnCount := 0
	for _, t := range r.Tables {
		switch t.Status {
		case StatusFail:
			failCount++
		case StatusWarn:
			warnCount++
		}
	}
	if failCount > 0 {
		return StatusFail
	}
	if warnCount > 0 {
		return StatusWarn
	}
	return StatusPass
}

func (r *Report) FailedTables() []TableReport {
	var result []TableReport
	for _, t := range r.Tables {
		if t.Status == StatusFail {
			result = append(result, t)
		}
	}
	return result
}

func (r *Report) SaveJSON(path string) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	// Atomic write (temp + rename): a crash mid-write must not leave a
	// truncated report.json behind for readers (compare /report endpoint).
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "report-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

func (r *Report) SaveText(path string) error {
	var lines []string
	lines = append(lines, fmt.Sprintf("=== %s Report ===", r.Phase))
	lines = append(lines, fmt.Sprintf("Time:     %s - %s (%s)", r.StartTime.Format(time.RFC3339), r.EndTime.Format(time.RFC3339), r.Duration))
	lines = append(lines, fmt.Sprintf("Status:   %s", r.Status))
	if r.Summary != "" {
		lines = append(lines, fmt.Sprintf("Summary:  %s", r.Summary))
	}
	lines = append(lines, fmt.Sprintf("Tables:   %d total, %d pass, %d fail, %d warn, %d skip",
		r.Stats.TotalTables, r.Stats.PassTables, r.Stats.FailTables, r.Stats.WarnTables, r.Stats.SkipTables))
	lines = append(lines, fmt.Sprintf("Rows:     source=%d target=%d diff=%d",
		r.Stats.TotalSourceRows, r.Stats.TotalTargetRows, r.Stats.TotalDiffRows))
	lines = append(lines, "")
	lines = append(lines, fmt.Sprintf("%-30s %-8s %12s %12s %12s %s", "Table", "Status", "Source Rows", "Target Rows", "Diff Rows", "Error"))
	lines = append(lines, strings.Repeat("-", 100))
	for _, t := range r.Tables {
		errStr := t.Error
		if len(errStr) > 40 {
			errStr = errStr[:40] + "..."
		}
		lines = append(lines, fmt.Sprintf("%-30s %-8s %12d %12d %12d %s", t.TableName, t.Status, t.SourceRows, t.TargetRows, t.DiffRows, errStr))
	}

	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0644)
}

func (r *Report) Save(path string) error {
	if strings.HasSuffix(path, ".json") {
		return r.SaveJSON(path)
	}
	if strings.HasSuffix(path, ".html") {
		return r.SaveHTML(path)
	}
	return r.SaveText(path)
}

func (r *Report) SaveHTML(path string) error {
	html := r.ToHTML()
	return os.WriteFile(path, []byte(html), 0644)
}

func (r *Report) ToHTML() string {
	var sb strings.Builder
	sb.WriteString(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>TiMS Migration Report</title>
<style>
` + ReportCSS + `
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
`)
	sb.WriteString(`<div class="header">
<h1><span class="logo">Ti</span>MS 迁移报告</h1>
<div class="subtitle">`)
	sb.WriteString(htmlEsc(r.Phase))
	sb.WriteString(` &middot; `)
	sb.WriteString(htmlEsc(r.StartTime.Format("2006-01-02 15:04:05")))
	sb.WriteString(` ~ `)
	sb.WriteString(htmlEsc(r.EndTime.Format("2006-01-02 15:04:05")))
	sb.WriteString(`</div>`)
	if r.TaskID != "" || r.Source != "" || r.Target != "" || r.Mode != "" {
		sb.WriteString(`<div class="meta">`)
		if r.TaskID != "" {
			sb.WriteString(fmt.Sprintf(`<div>任务 ID <span class="num">%s</span></div>`, htmlEsc(r.TaskID)))
		}
		if r.Source != "" {
			sb.WriteString(fmt.Sprintf(`<div>源端 <span>%s</span></div>`, htmlEsc(r.Source)))
		}
		if r.Target != "" {
			sb.WriteString(fmt.Sprintf(`<div>目标 <span>%s</span></div>`, htmlEsc(r.Target)))
		}
		if r.Mode != "" {
			sb.WriteString(fmt.Sprintf(`<div>迁移模式 <span>%s</span></div>`, htmlEsc(r.Mode)))
		}
		sb.WriteString(`</div>`)
	}
	sb.WriteString(`</div>`)

	statusClass := "overall-" + string(r.Status)
	sb.WriteString(`<div class="card"><h2>概览</h2><div class="stats">`)
	sb.WriteString(fmt.Sprintf(`<div class="stat"><div class="value %s">%s</div><div class="label">状态</div></div>`, statusClass, htmlEsc(statusCN(string(r.Status)))))
	sb.WriteString(fmt.Sprintf(`<div class="stat"><div class="value">%s</div><div class="label">耗时</div></div>`, htmlEsc(r.Duration)))
	sb.WriteString(fmt.Sprintf(`<div class="stat"><div class="value">%d</div><div class="label">总表数</div></div>`, r.Stats.TotalTables))
	sb.WriteString(fmt.Sprintf(`<div class="stat"><div class="value">%d</div><div class="label">源端行数</div></div>`, r.Stats.TotalSourceRows))
	sb.WriteString(fmt.Sprintf(`<div class="stat"><div class="value">%d</div><div class="label">目标行数</div></div>`, r.Stats.TotalTargetRows))
	sb.WriteString(`</div>`)
	if r.Summary != "" {
		sb.WriteString(fmt.Sprintf(`<div class="summary">%s</div>`, htmlEsc(r.Summary)))
	}
	sb.WriteString(`</div>`)

	// Stats card
	sb.WriteString(`<div class="card"><h2>统计</h2><div class="stats">`)
	sb.WriteString(fmt.Sprintf(`<div class="stat"><div class="value c-teal">%d</div><div class="label">通过</div></div>`, r.Stats.PassTables))
	sb.WriteString(fmt.Sprintf(`<div class="stat"><div class="value c-brand">%d</div><div class="label">失败</div></div>`, r.Stats.FailTables))
	sb.WriteString(fmt.Sprintf(`<div class="stat"><div class="value c-amber">%d</div><div class="label">警告</div></div>`, r.Stats.WarnTables))
	sb.WriteString(fmt.Sprintf(`<div class="stat"><div class="value c-muted">%d</div><div class="label">跳过</div></div>`, r.Stats.SkipTables))
	if r.Stats.TotalDiffRows != 0 {
		sb.WriteString(fmt.Sprintf(`<div class="stat"><div class="value c-brand">%d</div><div class="label">差异行数</div></div>`, r.Stats.TotalDiffRows))
	} else {
		sb.WriteString(`<div class="stat"><div class="value c-teal">0</div><div class="label">差异行数</div></div>`)
	}
	sb.WriteString(`</div></div>`)

	// Table detail card
	if len(r.Tables) > 0 {
		sb.WriteString(`<div class="card"><h2>表详情</h2><table><thead><tr>`)
		sb.WriteString(`<th>#</th><th>表名</th><th>状态</th><th>源端行数</th><th>目标行数</th><th>差异</th><th>耗时</th><th>错误</th>`)
		sb.WriteString(`</tr></thead><tbody>`)
		for i, t := range r.Tables {
			badgeClass := "badge-" + string(t.Status)
			errStr := htmlEsc(t.Error)
			if len(errStr) > 60 {
				errStr = errStr[:60] + "..."
			}
			diffStr := ""
			if t.DiffRows != 0 {
				diffStr = fmt.Sprintf("%d", t.DiffRows)
			}
			sb.WriteString(fmt.Sprintf(`<tr><td class="num">%d</td><td>%s</td><td><span class="badge %s">%s</span></td><td class="num">%d</td><td class="num">%d</td><td class="num">%s</td><td class="num">%s</td><td>%s</td></tr>`,
				i+1, htmlEsc(t.TableName), badgeClass, htmlEsc(statusCN(string(t.Status))), t.SourceRows, t.TargetRows, diffStr, htmlEsc(t.Duration), errStr))
		}
		sb.WriteString(`</tbody></table></div>`)
	}

	sb.WriteString(`<div class="footer">由 TiMS (TiDB Migration Suite) 生成 &middot; `)
	sb.WriteString(htmlEsc(time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString(`</div></div></body></html>`)

	return sb.String()
}

func htmlEsc(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return s
}

func statusCN(s string) string {
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
