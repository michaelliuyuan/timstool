package target

import (
	"fmt"
	"strings"
	"time"

	"github.com/michaelliuyuan/timstool/internal/source"
)

// RenderCSVRow renders one CIR Row as a TiDB-Lightning CSV line, mirroring the
// existing PG path's TSV format (TAB-separated, '\N' NULL, backslash-escape) so
// the SAME Lightning config (separator="\t", backslash-escape, null=\N, no
// header) reads it. Values render by Go kind — the source driver scans values
// into Go types, so this is source-agnostic. Pure (unit-testable).
// (doc multi-source-execution-engine-design §3, Lightning-from-CIR.)
func RenderCSVRow(cols []source.Column, row source.Row) string {
	b := &strings.Builder{}
	for i, c := range cols {
		if i > 0 {
			b.WriteByte('\t')
		}
		tv, ok := row[c.Name]
		if !ok || tv.Val == nil {
			b.WriteString(`\N`)
			continue
		}
		b.WriteString(escapeTSV(convertValue(tv.Val)))
	}
	b.WriteByte('\n')
	return b.String()
}

// convertValue renders a Go value (scanned by the source driver) as the CSV
// field text, mirroring internal/data.convertValue: nil→\N, bool→1/0,
// time→formatted, numbers→%v, []byte/string→raw. Source-agnostic (no PG-array
// conversion — non-PG sources don't have PG arrays).
func convertValue(v interface{}) string {
	if v == nil {
		return `\N`
	}
	switch x := v.(type) {
	case bool:
		if x {
			return "1"
		}
		return "0"
	case []byte:
		return string(x)
	case string:
		return x
	case time.Time:
		return x.Format("2006-01-02 15:04:05.999999")
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return fmt.Sprintf("%v", x)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// escapeTSV escapes a CSV field for the TAB-separated, backslash-escape format
// (mirrors internal/data.escapeTSV). The NULL marker '\N' is left as-is.
func escapeTSV(s string) string {
	if s == `\N` {
		return s
	}
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	return s
}
