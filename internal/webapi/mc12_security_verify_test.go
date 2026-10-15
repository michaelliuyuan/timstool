package webapi

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// MC-12 P2 anchors: POST /api/v1/security/verify rides the SAME gate as
// every destructive surface — unset security.token = 403 fail-closed;
// missing/wrong X-Auth-Token = the unified 401 body, byte-identical for
// missing vs wrong (no token-existence oracle, adversarial seq349 ②);
// correct snapshot = 200 {"status":"ok"}; every outcome lands in the
// security-core audit trail (cdc-ops.jsonl, action=security.verify).

func TestMC12SecurityVerifyGate(t *testing.T) {
	s, _, dir := mc10BareServer(t)

	// Zero new semantics, leg 0: unset token stays fail-closed 403.
	if rec := mc10Post(s, "/api/v1/security/verify", "", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("unset token: %d %s, want 403", rec.Code, rec.Body.String())
	}

	// Anchor 1: missing token → the unified 401 body.
	s.SetSecurityToken(testAuthToken)
	recNo := mc10Post(s, "/api/v1/security/verify", "", "")
	if recNo.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d %s, want 401", recNo.Code, recNo.Body.String())
	}
	if !strings.Contains(recNo.Body.String(), "unauthorized：X-Auth-Token 缺失或错误") {
		t.Fatalf("no token body: %s, want the unified gate wording", recNo.Body.String())
	}

	// Anchor 2: wrong token → byte-identical to the missing-token body
	// (anti-enumeration: the two denials are indistinguishable).
	recWrong := mc10Post(s, "/api/v1/security/verify", "", "wrong-token")
	if recWrong.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d %s, want 401", recWrong.Code, recWrong.Body.String())
	}
	if recWrong.Body.String() != recNo.Body.String() {
		t.Fatalf("401 bodies differ (existence oracle):\nmissing=%s\nwrong=%s", recNo.Body.String(), recWrong.Body.String())
	}

	// Anchor 3: correct token → 200 {"status":"ok"} and nothing else.
	recOK := mc10Post(s, "/api/v1/security/verify", "", testAuthToken)
	if recOK.Code != http.StatusOK {
		t.Fatalf("correct token: %d %s, want 200", recOK.Code, recOK.Body.String())
	}
	if strings.TrimSpace(recOK.Body.String()) != `{"status":"ok"}` {
		t.Fatalf("correct token body: %s, want {\"status\":\"ok\"}", recOK.Body.String())
	}

	// Anchor 4: every outcome audited in the security-core trail.
	assertAuditLine(t, dir, "cdc-ops.jsonl", `"action":"security.verify"`, `"result":"denied"`)
	assertAuditLine(t, dir, "cdc-ops.jsonl", `"action":"security.verify"`, `"result":"success"`)
	// Verify is a security-plane op, never a compare op: mc-ops.jsonl stays
	// untouched by this surface.
	if _, err := os.Stat(filepath.Join(dir, "audit", "mc-ops.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("mc-ops.jsonl written by security.verify: err=%v", err)
	}
}
