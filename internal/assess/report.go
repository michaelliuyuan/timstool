package assess

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/michaelliuyuan/timstool/internal/source"
)

// ReportGenerator creates assessment reports in various formats.
type ReportGenerator struct {
	report  *AssessmentReport
	srcMeta source.SourceMeta
	srcKind string
}

// NewReportGenerator creates a report generator from assessment results.
// srcType drives the source-database labels rendered in the reports; it is
// resolved through the source registry (A3 single truth: callers must not
// branch on the raw type string). "" normalizes to the PG legacy wording so
// inline/legacy call sites stay byte-identical; an unknown kind falls back to
// PG rather than failing report rendering.
func NewReportGenerator(dims []DimensionResult, srcType string) *ReportGenerator {
	kind, err := source.NormalizeKind(srcType)
	if err != nil {
		kind = "postgres"
	}
	meta, err := source.Describe(kind)
	if err != nil {
		meta = source.SourceMeta{DisplayName: "PostgreSQL", ShortName: "PG"}
	}
	return &ReportGenerator{report: buildReport(dims), srcMeta: meta, srcKind: kind}
}

// CaliberLine renders the item-4 assessment-caliber annotation: an
// explicit source-kind line for non-PG sources (the scoring caliber is
// always "TiDB target compatibility"). PG keeps the legacy layout with
// no extra line (report-face parity anchor).
func (rg *ReportGenerator) CaliberLine() string {
	if rg.srcKind == "mysql" {
		return fmt.Sprintf("源端类型：%s（口径：TiDB 目标兼容性）", rg.SourceLabel())
	}
	return ""
}

// SourceLabel returns the full source label used in report titles.
func (rg *ReportGenerator) SourceLabel() string {
	if rg.srcMeta.DisplayName != "" {
		return rg.srcMeta.DisplayName
	}
	return "PostgreSQL"
}

// SourceLabelShort returns the short source label used in table headers.
func (rg *ReportGenerator) SourceLabelShort() string {
	if rg.srcMeta.ShortName != "" {
		return rg.srcMeta.ShortName
	}
	return "PG"
}

func buildReport(dims []DimensionResult) *AssessmentReport {
	r := &AssessmentReport{
		DimensionResults: dims,
		Summary:          make(map[string]int),
	}

	// MS-10b2 item 3: N/A dimensions stay out of the weighted aggregate
	// (numerator AND denominator). With every dimension applicable the
	// weights sum to 1.0, so the division is a no-op for legacy payloads
	// (PG parity).
	var totalScore float64
	var totalWeight float64
	for _, dim := range dims {
		if !dim.IsApplicable() {
			continue
		}
		weight := DimensionWeights[dim.Dimension]
		totalScore += weight * dim.Score
		totalWeight += weight
		r.AllFindings = append(r.AllFindings, dim.Findings...)
		for _, f := range dim.Findings {
			r.Summary[f.Level]++
		}
	}
	if totalWeight > 0 {
		r.Score = totalScore / totalWeight
	}
	r.Level = OverallLevel(r.Score)

	return r
}

// Report returns the assessment report.
func (rg *ReportGenerator) Report() *AssessmentReport {
	return rg.report
}

// PrintTerminal outputs a colored terminal table to stdout.
func (rg *ReportGenerator) PrintTerminal(w io.Writer) {
	r := rg.report

	fmt.Fprintf(w, "\n")
	fmt.Fprintf(w, "╔══════════════════════════════════════════════════════════════╗\n")
	fmt.Fprintf(w, "║         %s → TiDB 兼容性评估报告                    ║\n", rg.SourceLabel())
	fmt.Fprintf(w, "╚══════════════════════════════════════════════════════════════╝\n")
	fmt.Fprintf(w, "\n")
	if caliber := rg.CaliberLine(); caliber != "" {
		fmt.Fprintf(w, "  %s\n", caliber)
		fmt.Fprintf(w, "\n")
	}

	// Overall score
	emoji := LevelEmoji[r.Level]
	fmt.Fprintf(w, "  总体评分: %s %s/100  (%s)\n", emoji, FormatScore(r.Score), levelNameCN(r.Level))
	fmt.Fprintf(w, "\n")

	// Summary
	fmt.Fprintf(w, "  摘要:\n")
	fmt.Fprintf(w, "    ✅ 兼容:     %d 项\n", r.Summary[LevelCompatible])
	fmt.Fprintf(w, "    ⚠️  可转换:   %d 项\n", r.Summary[LevelConvertible])
	fmt.Fprintf(w, "    🟡 需手动:   %d 项\n", r.Summary[LevelManualNeeded])
	fmt.Fprintf(w, "    ❌ 不兼容:   %d 项\n", r.Summary[LevelIncompatible])
	fmt.Fprintf(w, "\n")

	// Dimension scores
	fmt.Fprintf(w, "  ┌────────────────┬────────┬────────────────────────────────┐\n")
	fmt.Fprintf(w, "  │ 评估维度       │  得分  │ 说明                           │\n")
	fmt.Fprintf(w, "  ├────────────────┼────────┼────────────────────────────────┤\n")
	for _, dim := range r.DimensionResults {
		name := dimNameCN(dim.Dimension)
		if !dim.IsApplicable() {
			// MS-10b2 item 3: empty source object set renders N/A, not
			// a fake 100.
			fmt.Fprintf(w, "  │ %-12s   │ ➖ %-5s │ %-30s │\n",
				name, "N/A", "不适用（源端无此类对象）")
			continue
		}
		emoji := LevelEmoji[OverallLevel(dim.Score)]
		fmt.Fprintf(w, "  │ %-12s   │ %s %-5s │ %-30s │\n",
			name, emoji, FormatScore(dim.Score),
			fmt.Sprintf("共 %d 项", dim.Total))
	}
	fmt.Fprintf(w, "  └────────────────┴────────┴────────────────────────────────┘\n")
	fmt.Fprintf(w, "\n")

	// Problem items (non-compatible)
	var problems []Finding
	for _, f := range r.AllFindings {
		if f.Level != LevelCompatible {
			problems = append(problems, f)
		}
	}

	if len(problems) > 0 {
		// Sort: incompatible first, then manual, then convertible
		sort.Slice(problems, func(i, j int) bool {
			return levelOrder(problems[i].Level) < levelOrder(problems[j].Level)
		})

		fmt.Fprintf(w, "  需要处理的项目 (共 %d 项):\n", len(problems))
		fmt.Fprintf(w, "  ┌────┬────────────────────┬──────────┬─────────────────────────────────┐\n")
		fmt.Fprintf(w, "  │ #  │ 对象               │ 级别     │ 建议                            │\n")
		fmt.Fprintf(w, "  ├────┼────────────────────┼──────────┼─────────────────────────────────┤\n")

		maxShow := 50
		if len(problems) > maxShow {
			problems = problems[:maxShow]
		}

		for i, f := range problems {
			emoji := LevelEmoji[f.Level]
			objName := f.ObjectName
			if len(objName) > 18 {
				objName = "..." + objName[len(objName)-15:]
			}
			suggestion := f.Suggestion
			if len(suggestion) > 31 {
				suggestion = suggestion[:28] + "..."
			}
			if suggestion == "" {
				suggestion = "-"
			}
			fmt.Fprintf(w, "  │ %2d │ %-18s │ %s %-6s │ %-31s │\n",
				i+1, objName, emoji, levelShort(f.Level), suggestion)
		}
		fmt.Fprintf(w, "  └────┴────────────────────┴──────────┴─────────────────────────────────┘\n")
		fmt.Fprintf(w, "\n")
	}
}

// WriteJSON writes the report as JSON.
func (rg *ReportGenerator) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rg.report)
}

// WriteJSONFile writes the report as JSON to a file.
func (rg *ReportGenerator) WriteJSONFile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return rg.WriteJSON(f)
}

func levelNameCN(level string) string {
	switch level {
	case LevelCompatible:
		return "兼容"
	case LevelConvertible:
		return "可转换"
	case LevelManualNeeded:
		return "需手动处理"
	case LevelIncompatible:
		return "不兼容"
	default:
		return level
	}
}

func levelShort(level string) string {
	switch level {
	case LevelCompatible:
		return "兼容"
	case LevelConvertible:
		return "转换"
	case LevelManualNeeded:
		return "手动"
	case LevelIncompatible:
		return "不兼容"
	default:
		return level
	}
}

func levelOrder(level string) int {
	switch level {
	case LevelIncompatible:
		return 0
	case LevelManualNeeded:
		return 1
	case LevelConvertible:
		return 2
	case LevelCompatible:
		return 3
	default:
		return 4
	}
}

func dimNameCN(dim string) string {
	switch dim {
	case DimDataType:
		return "数据类型"
	case DimStructure:
		return "表结构"
	case DimIndex:
		return "索引"
	case DimView:
		return "视图"
	case DimFunction:
		return "函数"
	case DimTrigger:
		return "触发器"
	case DimCustomType:
		return "自定义类型"
	case DimExtension:
		return "扩展"
	case DimSequence:
		return "序列"
	default:
		return dim
	}
}
