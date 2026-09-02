package core

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yurimeng/cios/pkg/cpath"
)

func TestControlAudit_HTTPReadback(t *testing.T) {
	dict, err := cpath.LoadDict(filepath.Join(moduleRoot(t), "protocol"))
	if err != nil {
		t.Fatalf("LoadDict: %v", err)
	}
	st, err := NewFileStore(filepath.Join(t.TempDir(), "s.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServerWithStore(st, dict, "http://127.0.0.1:9", nil)
	h := srv.Handler()

	pathA := "sgp01.pod000.chiller000.compressor000.status"
	bodyA, _ := json.Marshal(SetRequest{Value: 1, TTLSeconds: 30})
	req := httptest.NewRequest(http.MethodPut, "/v1/points/"+pathA+":set", bytes.NewReader(bodyA))
	req = asPrincipal(req, ciOperator)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("A pending: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var pendResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &pendResp); err != nil {
		t.Fatalf("unmarshal pending: %v body=%s", err, rec.Body.String())
	}
	if pendResp["status"] != "pending" {
		t.Fatalf("want status=pending got %v", pendResp)
	}
	pendingID, _ := pendResp["pending_id"].(string)
	if pendingID == "" {
		t.Fatalf("no pending_id: %v", pendResp)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/control/"+pendingID+":approve", nil)
	req = asPrincipal(req, ciAdmin)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("approve: status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/control/audit", nil)
	req = asPrincipal(req, ciViewer)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET audit: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got listSetAuditsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if len(got.Items) != 2 {
		t.Fatalf("items len=%d want 2 items=%+v", len(got.Items), got.Items)
	}
	execRows := 0
	for _, item := range got.Items {
		if item.Second == ciAdmin {
			execRows++
			if item.Path != pathA || item.Actor != ciOperator || item.Class != RiskClassA {
				t.Fatalf("item=%+v", item)
			}
		}
	}
	if execRows != 1 {
		t.Fatalf("exec rows=%d items=%+v", execRows, got.Items)
	}
}

func TestControlAudit_PaginationNoSkip(t *testing.T) {
	dict, err := cpath.LoadDict(filepath.Join(moduleRoot(t), "protocol"))
	if err != nil {
		t.Fatalf("LoadDict: %v", err)
	}
	st, err := NewFileStore(filepath.Join(t.TempDir(), "s.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServerWithStore(st, dict, "http://127.0.0.1:9", nil)
	h := srv.Handler()

	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	path := "sgp01.pod000.chiller000.compressor000.status"
	recs := []SetAudit{
		{ID: "sa_a", Path: path, Class: RiskClassA, Value: 1, Actor: ciOperator, At: t0, Readback: true},
		{ID: "sa_b", Path: path, Class: RiskClassA, Value: 2, Actor: ciOperator, At: t0.Add(time.Second), Readback: true},
		{ID: "sa_c", Path: path, Class: RiskClassA, Value: 3, Actor: ciOperator, At: t0.Add(2 * time.Second), Readback: true},
	}
	ctx := context.Background()
	for _, a := range recs {
		if err := st.AppendSetAudit(ctx, a); err != nil {
			t.Fatalf("AppendSetAudit %s: %v", a.ID, err)
		}
	}

	wantIDs := []string{"sa_c", "sa_b", "sa_a"}
	var gotIDs []string
	seen := map[string]bool{}
	pageToken := ""
	for page := 0; page < 3; page++ {
		u := "/v1/control/audit?page_size=1"
		if pageToken != "" {
			u += "&page_token=" + pageToken
		}
		req := httptest.NewRequest(http.MethodGet, u, nil)
		req = asPrincipal(req, ciViewer)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: status=%d body=%s", page, rec.Code, rec.Body.String())
		}
		var got listSetAuditsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("page %d unmarshal: %v", page, err)
		}
		if len(got.Items) != 1 {
			t.Fatalf("page %d: items len=%d want 1", page, len(got.Items))
		}
		id := got.Items[0].ID
		if seen[id] {
			t.Fatalf("duplicate id %q across pages", id)
		}
		seen[id] = true
		gotIDs = append(gotIDs, id)
		pageToken = got.NextPageToken
		if page < 2 && pageToken == "" {
			t.Fatalf("page %d: empty page_token, want continuation", page)
		}
	}
	if pageToken != "" {
		t.Fatalf("last page still has page_token=%q", pageToken)
	}
	if len(gotIDs) != 3 {
		t.Fatalf("got %d ids want 3: %v", len(gotIDs), gotIDs)
	}
	for i, id := range wantIDs {
		if gotIDs[i] != id {
			t.Fatalf("order[%d]=%q want %q (got=%v)", i, gotIDs[i], id, gotIDs)
		}
	}
}

func TestControlAudit_FileStoreRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	st, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	want := SetAudit{
		ID:        "sa_restart",
		Path:      "sgp01.pod000.chiller000.compressor000.status",
		Class:     RiskClassA,
		Value:     42,
		Actor:     ciOperator,
		Second:    ciAdmin,
		At:        time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
		Readback:  true,
		Note:      "restart",
		RequestID: "rid-1",
	}
	if err := st.AppendSetAudit(context.Background(), want); err != nil {
		t.Fatalf("AppendSetAudit: %v", err)
	}
	st2, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := st2.ListSetAudits(context.Background())
	if err != nil {
		t.Fatalf("ListSetAudits: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len=%d want 1 got=%+v", len(got), got)
	}
	a := got[0]
	if a.ID != want.ID || a.Path != want.Path || a.Class != want.Class ||
		a.Value != want.Value || a.Actor != want.Actor || a.Second != want.Second ||
		a.Readback != want.Readback || a.Note != want.Note || a.RequestID != want.RequestID {
		t.Fatalf("fields mismatch got=%+v want=%+v", a, want)
	}
	if !a.At.Equal(want.At) {
		t.Fatalf("At got=%v want=%v", a.At, want.At)
	}
}

func TestControlAudit_PGParity(t *testing.T) {
	dsn := os.Getenv("CIOS_PG_DSN")
	if dsn == "" {
		t.Skip("CIOS_PG_DSN not set - skipping PG set-audit parity test")
	}
	ctx := context.Background()
	mig := filepath.Join(moduleRoot(t), "migrations")
	st1, err := NewPGStore(ctx, dsn, mig)
	if err != nil {
		t.Fatalf("NewPGStore #1: %v", err)
	}
	want := SetAudit{
		ID:        newUsageID(),
		Path:      "sgp01.pod000.chiller000.compressor000.status",
		Class:     RiskClassA,
		Value:     7.5,
		Actor:     ciOperator,
		Second:    ciAdmin,
		At:        time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
		Readback:  true,
		Note:      "pg-parity",
		RequestID: "rid-pg",
	}
	if err := st1.AppendSetAudit(ctx, want); err != nil {
		t.Fatalf("AppendSetAudit: %v", err)
	}
	st2, err := NewPGStore(ctx, dsn, mig)
	if err != nil {
		t.Fatalf("NewPGStore #2: %v", err)
	}
	list, err := st2.ListSetAudits(ctx)
	if err != nil {
		t.Fatalf("ListSetAudits: %v", err)
	}
	var found *SetAudit
	for i := range list {
		if list[i].ID == want.ID {
			found = &list[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("id %q not in ListSetAudits (n=%d)", want.ID, len(list))
	}
	if found.Path != want.Path || found.Class != want.Class ||
		found.Value != want.Value || found.Actor != want.Actor || found.Second != want.Second ||
		found.Readback != want.Readback || found.Note != want.Note || found.RequestID != want.RequestID {
		t.Fatalf("fields mismatch got=%+v want=%+v", *found, want)
	}
	if !found.At.Equal(want.At) {
		t.Fatalf("At got=%v want=%v", found.At, want.At)
	}
}
