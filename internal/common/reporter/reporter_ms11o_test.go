package reporter

import (
	"strings"
	"testing"
)

// TestBadgeClassEscaped (MS-11o A3): badgeClass rides the htmlEsc face.
// Status is an internal enum with no user-facing channel today, so this is
// pure defense — a hostile or legacy Status value must never break out of
// the class attribute, and benign statuses must render byte-identically.
func TestBadgeClassEscaped(t *testing.T) {
	r := NewReport("data")
	r.AddTableReport(TableReport{
		TableName: "t_evil",
		Status:    Status(`pass"><script>alert(1)</script>`),
		Duration:  "1.000s",
	})
	r.AddTableReport(TableReport{TableName: "t_ok", Status: StatusPass, Duration: "2.000s"})
	r.Finish(StatusWarn, "ms11o anchor")

	html := r.ToHTML()
	if strings.Contains(html, `<script>alert(1)</script>`) {
		t.Fatal("raw script leaked into the HTML output")
	}
	if !strings.Contains(html, `badge-pass&quot;&gt;&lt;script&gt;`) {
		t.Errorf("escaped badgeClass form missing from the class attribute:\n%s", html)
	}
	if !strings.Contains(html, `class="badge badge-pass"`) {
		t.Errorf("benign badge rendering must stay byte-identical:\n%s", html)
	}
}
