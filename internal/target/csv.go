package target

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/michaelliuyuan/timstool/internal/source"
)

// RenderCSVRow renders one CIR Row as a TiDB-Lightning / LOAD-DATA CSV line:
// fields comma-separated, '\N' for NULL, RFC-4180-style quoting. Column order
// follows cols. The source driver scans values into Go types; we render by Go
// kind, which is correct across the type matrix (doc multi-source-execution-
// engine-design §3, Lightning-from-CIR). Pure (unit-testable).
func RenderCSVRow(cols []source.Column, row source.Row) string {
	b := &strings.Builder{}
	for i, c := range cols {
		if i > 0 {
			b.WriteByte(',')
		}
		tv, ok := row[c.Name]
		if !ok || tv.Val == nil {
			b.WriteString(`\N`)
			continue
		}
		writeCSVField(b, tv.Val)
	}
	b.WriteByte('\n')
	return b.String()
}

func writeCSVField(b *strings.Builder, v interface{}) {
	switch x := v.(type) {
	case nil:
		b.WriteString(`\N`)
	case []byte:
		// binary (BIT/BLOB/VARBINARY): PG-COPY bytea style \xhex, the format the
		// Lightning CSV reader (configured for the PG path) accepts.
		b.WriteString(`\x`)
		b.WriteString(hex.EncodeToString(x))
	case string:
		writeCSVString(b, x)
	case bool:
		// MySQL BOOLEAN is TINYINT(1); store as 0/1.
		if x {
			b.WriteByte('1')
		} else {
			b.WriteByte('0')
		}
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case int:
		b.WriteString(strconv.Itoa(x))
	case int32:
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case float64:
		b.WriteString(strconv.FormatFloat(x, 'f', -1, 64))
	case float32:
		b.WriteString(strconv.FormatFloat(float64(x), 'f', -1, 32))
	case time.Time:
		if x.IsZero() {
			b.WriteString(`\N`)
		} else {
			b.WriteString(x.Format("2006-01-02 15:04:05.999999"))
		}
	default:
		writeCSVString(b, fmt.Sprint(v))
	}
}

// writeCSVString writes a string field, RFC-4180-quoting when it contains a
// comma, quote, newline/CR, or would be mistaken for the NULL marker (\N).
func writeCSVString(b *strings.Builder, s string) {
	if s == "" {
		b.WriteString(`""`) // distinguish empty string from NULL (\N)
		return
	}
	if !needsQuote(s) {
		b.WriteString(s)
		return
	}
	b.WriteByte('"')
	b.WriteString(strings.ReplaceAll(s, `"`, `""`))
	b.WriteByte('"')
}

func needsQuote(s string) bool {
	if s == `\N` {
		return true
	}
	return strings.ContainsAny(s, `,"`+"\n\r")
}
