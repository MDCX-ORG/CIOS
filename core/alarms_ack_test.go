// Package core — alarms_ack_test.go: PRMT-230 acceptance for
// POST /v1/alarms/{id}:ack (spec-003 §4 firing→acked, one-way).
//
// Identity discipline (fixed CI accounts): every case injects one of
// the closed fixed accounts svc:ci-admin / svc:ci-operator /
// svc:ci-viewer through a file-local static token verifier. No
// case asserts a value derived from "no principal"; the single
// no-principal case (TestAlarmAck_NoPrincipal_FailsClosed) tests
// the handler's fail-closed guard branch, not an identity meaning.
package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// buildAckVerifier returns a verifier holding the three fixed CI
// accounts (fixed CI account table) plus their plaintext tokens.
// Built locally rather than reusing buildR2Verifier because that
// helper's subjects (svc:viewer/operator/admin) are the old names
// the discipline forbids for new tests.
func buildAckVerifier(t *testing.T, operatorScopes []string) (TokenVerifier, string, string, string) {
	t.Helper()
	const (
		viewerTok   = "ack-ci-viewer-token"
		operatorTok = "ack-ci-operator-token"
		adminTok    = "ack-ci-admin-token"
	)
	h := func(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
	v, err := NewStaticTokenVerifier(map[string]Principal{
		h(viewerTok):   {Subject: "svc:ci-viewer", Role: RoleViewer, Scopes: []string{"site01.**"}},
		h(operatorTok): {Subject: "svc:ci-operator", Role: RoleOperator, Scopes: operatorScopes},
		h(adminTok):    {Subject: "svc:ci-admin", Role: RoleAdmin, Scopes: []string{"**"}},
	})
	if err != nil {
		t.Fatalf("NewStaticTokenVerifier: %v", err)
	}
	return v, viewerTok, operatorTok, adminTok
}

// newAckServer builds an authed server with A1 firing and A3
// resolved already seeded, plus its httptest front door.
func newAckServer(t *testing.T, operatorScopes []string) (*httptest.Server, string, string, string) {
	t.Helper()
	v, viewerTok, operatorTok, adminTok := buildAckVerifier(t, operatorScopes)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	if err := srv.st.SeedAlarms(context.Background(), []Alarm{
		{ID: "A1", Path: "site01.pod000.cdu000", Severity: "critical", State: "firing", Summary: "hot", Since: time.Now().UTC()},
		{ID: "A3", Path: "site01.pod000.cdu000", Severity: "minor", State: "resolved", Summary: "gone", Since: time.Now().UTC()},
	}); err != nil {
		t.Fatalf("SeedAlarms: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, viewerTok, operatorTok, adminTok
}

// doAck POSTs {id}:ack with the given token ("" omits the header)
// and returns status + raw body.
func doAck(t *testing.T, ts *httptest.Server, method, path, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// Case 1: operator scoped to site01.** acks A1 → 200 with the
// persisted actor + timestamp, and the list reflects it.
func TestAlarmAck_Operator_FiringToAcked(t *testing.T) {
	ts, _, operatorTok, _ := newAckServer(t, []string{"site01.**"})

	code, body := doAck(t, ts, http.MethodPost, "/v1/alarms/A1:ack", operatorTok)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", code, body)
	}
	var got Alarm
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if got.State != "acked" {
		t.Errorf("State=%q, want acked", got.State)
	}
	if got.AckedBy != "svc:ci-operator" {
		t.Errorf("AckedBy=%q, want svc:ci-operator", got.AckedBy)
	}
	if got.AckedAt == nil || got.AckedAt.IsZero() {
		t.Errorf("AckedAt=%v, want non-null", got.AckedAt)
	}

	var list listAlarmsResp
	code, body = doAuthedGet(t, ts, "/v1/alarms", operatorTok, &list)
	if code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", code, body)
	}
	var seen bool
	for _, a := range list.Items {
		if a.ID == "A1" {
			seen = true
			if a.State != "acked" || a.AckedBy != "svc:ci-operator" {
				t.Errorf("listed A1 = %+v, want acked by svc:ci-operator", a)
			}
		}
	}
	if !seen {
		t.Errorf("A1 missing from the list after ack")
	}
}

// Case 2: re-ack of an already-acked alarm → 409 conflict.
func TestAlarmAck_ReAck_Conflict(t *testing.T) {
	ts, _, operatorTok, _ := newAckServer(t, []string{"site01.**"})

	if code, body := doAck(t, ts, http.MethodPost, "/v1/alarms/A1:ack", operatorTok); code != http.StatusOK {
		t.Fatalf("first ack status=%d body=%s, want 200", code, body)
	}
	code, body := doAck(t, ts, http.MethodPost, "/v1/alarms/A1:ack", operatorTok)
	if code != http.StatusConflict {
		t.Fatalf("re-ack status=%d body=%s, want 409", code, body)
	}
	if !strings.Contains(body, "conflict") {
		t.Errorf("body=%s, want problem type tail conflict", body)
	}
}

// Case 3: ack of a resolved alarm → 409 conflict (firing→acked is
// the only ack transition, spec-003 §4).
func TestAlarmAck_Resolved_Conflict(t *testing.T) {
	ts, _, operatorTok, _ := newAckServer(t, []string{"site01.**"})

	code, body := doAck(t, ts, http.MethodPost, "/v1/alarms/A3:ack", operatorTok)
	if code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409", code, body)
	}
	if !strings.Contains(body, "conflict") {
		t.Errorf("body=%s, want problem type tail conflict", body)
	}
}

// Case 4: viewer is below the ControlWrite role floor the
// middleware applies via isListScopeEndpoint → 403.
func TestAlarmAck_Viewer_Forbidden(t *testing.T) {
	ts, viewerTok, _, _ := newAckServer(t, []string{"site01.**"})

	code, body := doAck(t, ts, http.MethodPost, "/v1/alarms/A1:ack", viewerTok)
	if code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s, want 403", code, body)
	}
}

// Case 5: no Authorization header → 401 (auth is enabled).
func TestAlarmAck_NoToken_Unauthorized(t *testing.T) {
	ts, _, _, _ := newAckServer(t, []string{"site01.**"})

	code, body := doAck(t, ts, http.MethodPost, "/v1/alarms/A1:ack", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s, want 401", code, body)
	}
}

// Case 6: operator whose scope does not cover the alarm's Path →
// 403 from the handler's authorize() re-check (L50 explicit scope).
// The middleware only role-floors here, so a pass would mean the
// re-check is missing.
func TestAlarmAck_OperatorOutOfScope_Forbidden(t *testing.T) {
	ts, _, operatorTok, _ := newAckServer(t, []string{"site99.**"})

	code, body := doAck(t, ts, http.MethodPost, "/v1/alarms/A1:ack", operatorTok)
	if code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s, want 403 (handler scope re-check)", code, body)
	}
}

// Case 7: unknown alarm id → 404 path-not-found.
func TestAlarmAck_UnknownID_NotFound(t *testing.T) {
	ts, _, operatorTok, _ := newAckServer(t, []string{"site01.**"})

	code, body := doAck(t, ts, http.MethodPost, "/v1/alarms/NOPE:ack", operatorTok)
	if code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want 404", code, body)
	}
	if !strings.Contains(body, "path-not-found") {
		t.Errorf("body=%s, want problem type tail path-not-found", body)
	}
}

// Case 8: GET on the ack sub-resource → 405 from the handler's
// method gate. svc:ci-admin is used deliberately: isListScopeEndpoint
// is POST-only (PRMT-230 §3.4), so a GET takes the middleware's full
// authorize() branch, where mapRequest hands it the literal id
// "A1:ack" as the path — a scoped operator is 403'd there and never
// reaches the handler. Admin bypasses scope, so this case actually
// exercises the 405 the PRMT specifies. (The scoped-operator GET
// 403 is recorded as a spec doubt in the Implementation Notes.)
func TestAlarmAck_GET_MethodNotAllowed(t *testing.T) {
	ts, _, _, adminTok := newAckServer(t, []string{"site01.**"})

	code, body := doAck(t, ts, http.MethodGet, "/v1/alarms/A1:ack", adminTok)
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d body=%s, want 405", code, body)
	}
}

// Case 9: direct handler call with a context that carries NO
// principal. Per the CI identity discipline this asserts the handler's
// fail-closed guard BRANCH (a production binary booted with auth
// disabled must not ack), NOT any identity meaning of "no
// principal" — no synthesized subject is asserted anywhere.
func TestAlarmAck_NoPrincipal_FailsClosed(t *testing.T) {
	srv := newAuthTestServer(t, nil)
	if err := srv.st.SeedAlarms(context.Background(), []Alarm{
		{ID: "A1", Path: "site01.pod000.cdu000", Severity: "critical", State: "firing", Since: time.Now().UTC()},
	}); err != nil {
		t.Fatalf("SeedAlarms: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/alarms/A1:ack", nil)
	w := httptest.NewRecorder()
	srv.serveAlarmAck(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s, want 401 (fail-closed guard)", w.Code, w.Body.String())
	}
	all, err := srv.st.ListAlarms(context.Background())
	if err != nil {
		t.Fatalf("ListAlarms: %v", err)
	}
	for _, a := range all {
		if a.ID == "A1" && a.State != "firing" {
			t.Errorf("A1 state=%q after a rejected ack, want firing (no write may happen)", a.State)
		}
	}
}

// Case 10: fileStore.AckAlarm unit contract — not-found, not-firing,
// and persistence across a NewFileStore reload.
func TestFileStoreAckAlarm_Contract(t *testing.T) {
	path := t.TempDir() + "/store.json"
	st, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	ctx := context.Background()
	if err := st.SeedAlarms(ctx, []Alarm{
		{ID: "A1", Path: "site01.pod000.cdu000", Severity: "critical", State: "firing", Since: time.Now().UTC()},
		{ID: "A3", Path: "site01.pod000.cdu000", Severity: "minor", State: "resolved", Since: time.Now().UTC()},
	}); err != nil {
		t.Fatalf("SeedAlarms: %v", err)
	}

	if _, found, err := st.AckAlarm(ctx, "MISSING", "svc:ci-operator"); found || err != nil {
		t.Errorf("AckAlarm(MISSING) = (_, %v, %v), want (_, false, nil)", found, err)
	}

	cur, found, err := st.AckAlarm(ctx, "A3", "svc:ci-operator")
	if !found || err != ErrAlarmNotAckable {
		t.Errorf("AckAlarm(A3) = (_, %v, %v), want (row, true, ErrAlarmNotAckable)", found, err)
	}
	if cur.State != "resolved" {
		t.Errorf("returned row State=%q, want resolved (caller builds the 409 detail from it)", cur.State)
	}

	out, found, err := st.AckAlarm(ctx, "A1", "svc:ci-operator")
	if !found || err != nil {
		t.Fatalf("AckAlarm(A1) = (_, %v, %v), want (row, true, nil)", found, err)
	}
	if out.State != "acked" || out.AckedBy != "svc:ci-operator" || out.AckedAt == nil {
		t.Fatalf("AckAlarm(A1) row = %+v, want acked/svc:ci-operator/non-nil AckedAt", out)
	}

	reloaded, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("reload NewFileStore: %v", err)
	}
	all, err := reloaded.ListAlarms(ctx)
	if err != nil {
		t.Fatalf("ListAlarms after reload: %v", err)
	}
	var seen bool
	for _, a := range all {
		if a.ID == "A1" {
			seen = true
			if a.State != "acked" || a.AckedBy != "svc:ci-operator" || a.AckedAt == nil {
				t.Errorf("reloaded A1 = %+v, want acked/svc:ci-operator/non-nil AckedAt", a)
			}
		}
	}
	if !seen {
		t.Errorf("A1 missing after reload")
	}
}
