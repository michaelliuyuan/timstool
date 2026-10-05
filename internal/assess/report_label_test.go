package assess

import (
	"bytes"
	"strings"
	"testing"

	// The label resolution goes through the source registry, which the
	// adapters populate via init() — blank-import them so the test binary
	// has the real meta (production binaries import them via webapi/cmd).
	_ "github.com/michaelliuyuan/timstool/internal/source/mysql"
	_ "github.com/michaelliuyuan/timstool/internal/source/postgres"
)

// MS-10c 笔⑤姊妹锚（FE assessDynamicLabel.spec.ts 家族同构）：报告源库标签必须
// 随 srcType 动态分派，Go 侧 HTML/terminal 模板禁 PG/PostgreSQL 硬编码泄漏到
// MySQL 源产物（adversarial P1-1）。
func TestHTMLReportDynamicLabels(t *testing.T) {
	dims := []DimensionResult{
		{
			Dimension: DimDataType,
			Score:     100,
			Total:     1,
			Findings: []Finding{
				{Level: LevelCompatible, ObjectType: "column", ObjectName: "t1.a"},
				{Level: LevelManualNeeded, ObjectType: "column", ObjectName: "t1.b", PGDetail: "text", TiDBDetail: "text", Suggestion: "review"},
			},
		},
	}

	render := func(srcType string) string {
		var buf bytes.Buffer
		rg := NewReportGenerator(dims, srcType)
		if err := rg.WriteHTML(&buf); err != nil {
			t.Fatalf("WriteHTML(%q): %v", srcType, err)
		}
		return buf.String()
	}

	t.Run("mysql source labels", func(t *testing.T) {
		html := render("mysql")
		if !strings.Contains(html, "<title>MySQL → TiDB 兼容性评估报告</title>") {
			t.Errorf("title not dynamic for mysql source")
		}
		if !strings.Contains(html, "<h1>MySQL → TiDB 兼容性评估报告</h1>") {
			t.Errorf("h1 not dynamic for mysql source")
		}
		if !strings.Contains(html, "<th>MySQL</th>") {
			t.Errorf("problem table source header not dynamic for mysql source")
		}
		if strings.Contains(html, "PostgreSQL") {
			t.Errorf("mysql report leaked PostgreSQL label")
		}
	})

	t.Run("pg legacy byte-identity", func(t *testing.T) {
		for _, srcType := range []string{"postgres", ""} {
			html := render(srcType)
			if !strings.Contains(html, "<title>PostgreSQL → TiDB 兼容性评估报告</title>") {
				t.Errorf("title changed for PG source (%q)", srcType)
			}
			if !strings.Contains(html, "<h1>PostgreSQL → TiDB 兼容性评估报告</h1>") {
				t.Errorf("h1 changed for PG source (%q)", srcType)
			}
			if !strings.Contains(html, "<th>PG</th>") {
				t.Errorf("problem table header changed for PG source (%q)", srcType)
			}
		}
	})
}

func TestTerminalReportDynamicLabels(t *testing.T) {
	dims := []DimensionResult{
		{
			Dimension: DimDataType,
			Score:     100,
			Total:     1,
			Findings: []Finding{
				{Level: LevelCompatible, ObjectType: "column", ObjectName: "t1.a"},
			},
		},
	}

	var buf bytes.Buffer
	rg := NewReportGenerator(dims, "mysql")
	rg.PrintTerminal(&buf)
	out := buf.String()
	if !strings.Contains(out, "MySQL → TiDB 兼容性评估报告") {
		t.Errorf("terminal title not dynamic for mysql source")
	}

	buf.Reset()
	rg = NewReportGenerator(dims, "")
	rg.PrintTerminal(&buf)
	if !strings.Contains(buf.String(), "PostgreSQL → TiDB 兼容性评估报告") {
		t.Errorf("terminal title changed for PG source")
	}
}
