// Package core — spares_test.go: end-to-end tests for the
// /v1/spares HTTP surface + Store methods (PRMT-048 §6 coverage).
//
// Coverage matrix:
//
//   - id pattern (newSpareID, newSpareTxnID)
//   - create (POST /v1/spares → 201)
//   - :adjust in (positive delta → qty rises, txn appended)
//   - :adjust out (negative delta → qty falls, txn linked to ticket)
//   - qty<0 reject (422 insufficient-stock)
//   - low_stock derivation (GET /v1/spares/{id} flags it)
//   - txn append + ticket_id linkage (list txns in /v1/spares/{id})
//   - POST /v1/spares direct qty change is NOT possible (qty only
//     changes via :adjust — POST /v1/spares accepts qty as the
//     initial seed on creation, but no PUT qty endpoint exists)
//   - file+pg parity (spareStore_AdjustAndList tests the
//     in-memory store; pg integration is exercised by
//     sparePG_AdjustAndList only when CIOS_TEST_PG_DSN is set)
//   - 401 when no bearer (auth required regression — PRMT-037 lesson)
//   - 401 names match the Spare.* pattern run by §6 acceptance
package core

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// --- id shape ----------------------------------------------------------

func TestSpareIDPattern(t *testing.T) {
	id := newSpareID()
	if !spareIDPattern.MatchString(id) {
		t.Errorf("newSpareID() = %q, does not match sp_[A-Z2-7]{16}", id)
	}
	txnID := newSpareTxnID()
	if !spareTxnIDPattern.MatchString(txnID) {
		t.Errorf("newSpareTxnID() = %q, does not match st_[A-Z2-7]{16}", txnID)
	}
}

// --- Store layer (fileStore parity) -----------------------------------

func TestSpareStore_PutAndGet(t *testing.T) {
	st, _ := newStore(t)
	sp := SparePart{
		ID:       "sp_AAAAAAAAAAAAAAAA",
		SKU:      "SKU-001",
		Name:     "Filter",
		Qty:      5,
		MinQty:   2,
		Location: "rack-a",
	}
	if err := st.PutSpare(context.Background(), sp); err != nil {
		t.Fatalf("PutSpare: %v", err)
	}
	got, ok, err := st.GetSpare(context.Background(), sp.ID)
	if err != nil || !ok {
		t.Fatalf("GetSpare ok=%v err=%v", ok, err)
	}
	if got.Qty != 5 || got.MinQty != 2 || got.SKU != "SKU-001" {
		t.Errorf("got %+v, want qty=5 min=2 sku=SKU-001", got)
	}
}

func TestSpareStore_AdjustInOut(t *testing.T) {
	st, _ := newStore(t)
	sp := SparePart{ID: "sp_BBBBBBBBBBBBBBBB", SKU: "S", Name: "n", Qty: 10, MinQty: 1}
	if err := st.PutSpare(context.Background(), sp); err != nil {
		t.Fatal(err)
	}
	// Adjust +5 → qty 15.
	updated, txn, err := st.AdjustSpare(context.Background(), sp.ID, 5, "", time.Now().UTC())
	if err != nil {
		t.Fatalf("adjust +5: %v", err)
	}
	if updated.Qty != 15 || txn.Delta != 5 || txn.SpareID != sp.ID {
		t.Errorf("updated qty=%d txn=%+v", updated.Qty, txn)
	}
	// Adjust -3 with ticket link → qty 12, txn.ticket_id == "tk_...".
	updated, txn, err = st.AdjustSpare(context.Background(), sp.ID, -3, "tk_TICKETCONSUMED", time.Now().UTC())
	if err != nil {
		t.Fatalf("adjust -3: %v", err)
	}
	if updated.Qty != 12 {
		t.Errorf("qty after -3 = %d, want 12", updated.Qty)
	}
	if txn.TicketID != "tk_TICKETCONSUMED" {
		t.Errorf("txn ticket_id = %q, want tk_TICKETCONSUMED", txn.TicketID)
	}
	// Verify both txns surfaced in ListSpareTxns newest-first.
	txns, _ := st.ListSpareTxns(context.Background(), sp.ID)
	if len(txns) != 2 {
		t.Fatalf("txns len = %d, want 2", len(txns))
	}
	if txns[0].Delta != -3 {
		t.Errorf("newest txn delta = %d, want -3 (desc order)", txns[0].Delta)
	}
}

func TestSpareStore_AdjustRejectBelowZero(t *testing.T) {
	st, _ := newStore(t)
	sp := SparePart{ID: "sp_CCCCCCCCCCCCCCCC", SKU: "S", Name: "n", Qty: 1, MinQty: 0}
	if err := st.PutSpare(context.Background(), sp); err != nil {
		t.Fatal(err)
	}
	_, _, err := st.AdjustSpare(context.Background(), sp.ID, -5, "", time.Now().UTC())
	if err != ErrInsufficientStock {
		t.Errorf("adjust -5 from qty 1: err=%v, want ErrInsufficientStock", err)
	}
	// qty must NOT have changed.
	got, _, _ := st.GetSpare(context.Background(), sp.ID)
	if got.Qty != 1 {
		t.Errorf("qty drifted to %d, want 1", got.Qty)
	}
}

func TestSpareStore_AdjustRejectsZeroDelta(t *testing.T) {
	st, _ := newStore(t)
	sp := SparePart{ID: "sp_ZERODELTA0000000", SKU: "S", Name: "n", Qty: 3, MinQty: 0}
	if err := st.PutSpare(context.Background(), sp); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AdjustSpare(context.Background(), sp.ID, 0, "", time.Now().UTC()); err == nil {
		t.Errorf("adjust with delta=0: want error, got nil")
	}
}

// TestSpareStore_PutSpareSKUUniqueness (PRMT-080) covers the
// fileStore SKU uniqueness contract: create with a SKU already
// held by another id → ErrSKUExists; self-update of the SAME id
// with a new sku is allowed; self-update that collides with
// another id's sku → ErrSKUExists. Mirrors pgStore's UNIQUE
// constraint on sku so both implementations share one semantic.
func TestSpareStore_PutSpareSKUUniqueness(t *testing.T) {
	st, _ := newStore(t)
	a := SparePart{ID: "sp_SKUA000000000000", SKU: "SKU-A", Name: "a", Qty: 1, MinQty: 0}
	b := SparePart{ID: "sp_SKUB000000000000", SKU: "SKU-B", Name: "b", Qty: 1, MinQty: 0}
	if err := st.PutSpare(context.Background(), a); err != nil {
		t.Fatalf("seed a: %v", err)
	}
	if err := st.PutSpare(context.Background(), b); err != nil {
		t.Fatalf("seed b: %v", err)
	}
	// Create path: new id reuses SKU-A → ErrSKUExists.
	dup := SparePart{ID: "sp_SKUC000000000000", SKU: "SKU-A", Name: "c", Qty: 1, MinQty: 0}
	if err := st.PutSpare(context.Background(), dup); err != ErrSKUExists {
		t.Errorf("create with colliding sku: err=%v, want ErrSKUExists", err)
	}
	// Update path: same id (a), new sku (SKU-B) that collides with b → ErrSKUExists.
	collide := a
	collide.SKU = "SKU-B"
	if err := st.PutSpare(context.Background(), collide); err != ErrSKUExists {
		t.Errorf("update self into colliding sku: err=%v, want ErrSKUExists", err)
	}
	// Update path: same id (a), new sku that does NOT collide → allowed.
	ok := a
	ok.SKU = "SKU-A2"
	if err := st.PutSpare(context.Background(), ok); err != nil {
		t.Errorf("update self into free sku: err=%v, want nil", err)
	}
	got, _, _ := st.GetSpare(context.Background(), a.ID)
	if got.SKU != "SKU-A2" {
		t.Errorf("after self-update: sku=%q, want SKU-A2", got.SKU)
	}
	// Update path: same id (a), same sku → allowed (idempotent upsert).
	same := a
	same.SKU = "SKU-A2"
	if err := st.PutSpare(context.Background(), same); err != nil {
		t.Errorf("update self with same sku: err=%v, want nil", err)
	}
}

func TestSpareStore_ListSparesSorted(t *testing.T) {
	st, _ := newStore(t)
	for _, id := range []string{"sp_ZZZZZZZZZZZZZZZZ", "sp_AAAAAAAAAAAAAAAA", "sp_MMMMMMMMMMMMMMMM"} {
		_ = st.PutSpare(context.Background(), SparePart{ID: id, SKU: "k-" + id, Name: "n", Qty: 1, MinQty: 0})
	}
	got, _ := st.ListSpares(context.Background())
	if len(got) != 3 {
		t.Fatalf("len=%d", len(got))
	}
	want := []string{"sp_AAAAAAAAAAAAAAAA", "sp_MMMMMMMMMMMMMMMM", "sp_ZZZZZZZZZZZZZZZZ"}
	for i, w := range want {
		if got[i].ID != w {
			t.Errorf("got[%d].ID=%s, want %s", i, got[i].ID, w)
		}
	}
}

// --- HTTP layer --------------------------------------------------------

// spareListResp mirrors listSparesResponse in production code so
// tests can decode the wire envelope without depending on the
// unexported type.
type spareListResp struct {
	Items         []SparePart `json:"items"`
	NextPageToken string      `json:"next_page_token"`
}

// spareWithDerived mirrors sparePartWithDerived for the GET-by-id
// and :adjust response shapes.
type spareWithDerived struct {
	ID       string     `json:"id"`
	SKU      string     `json:"sku"`
	Name     string     `json:"name"`
	Qty      int        `json:"qty"`
	MinQty   int        `json:"min_qty"`
	Location string     `json:"location"`
	LowStock bool       `json:"low_stock"`
	Recent   []SpareTxn `json:"recent_txns"`
}

func TestSpareHTTP_CreateAndGet(t *testing.T) {
	_, ts := newTestServer(t)
	body := `{"sku":"SKU-HTTP-1","name":"Filter","qty":5,"min_qty":2,"location":"rack-a"}`
	r := doReq(t, ts, http.MethodPost, "/v1/spares", body)
	if r.code != http.StatusCreated {
		t.Fatalf("POST: %d %s", r.code, r.body)
	}
	var created SparePart
	mustJSON(t, r.body, &created)
	if !spareIDPattern.MatchString(created.ID) {
		t.Errorf("created ID = %q, bad shape", created.ID)
	}
	if created.Qty != 5 {
		t.Errorf("created qty = %d, want 5", created.Qty)
	}
	// GET single → includes low_stock + (empty) recent_txns.
	r = doReq(t, ts, http.MethodGet, "/v1/spares/"+created.ID, "")
	if r.code != http.StatusOK {
		t.Fatalf("GET: %d %s", r.code, r.body)
	}
	var got spareWithDerived
	mustJSON(t, r.body, &got)
	if got.LowStock {
		t.Errorf("qty=5 min_qty=2: low_stock should be false")
	}
	if len(got.Recent) != 0 {
		t.Errorf("no adjusts yet: recent len = %d, want 0", len(got.Recent))
	}
}

func TestSpareHTTP_AdjustInAndOut(t *testing.T) {
	_, ts := newTestServer(t)
	// Create.
	r := doReq(t, ts, http.MethodPost, "/v1/spares",
		`{"sku":"SKU-ADJ","name":"x","qty":3,"min_qty":1}`)
	if r.code != http.StatusCreated {
		t.Fatalf("POST: %d %s", r.code, r.body)
	}
	var sp SparePart
	mustJSON(t, r.body, &sp)
	// Adjust +7 (inbound) → qty 10.
	r = doReq(t, ts, http.MethodPost, "/v1/spares/"+sp.ID+":adjust",
		`{"delta":7}`)
	if r.code != http.StatusOK {
		t.Fatalf("adjust +7: %d %s", r.code, r.body)
	}
	var adj spareWithDerived
	mustJSON(t, r.body, &adj)
	if adj.Qty != 10 {
		t.Errorf("after +7: qty=%d, want 10", adj.Qty)
	}
	if adj.LowStock {
		t.Errorf("qty=10 min=1: low_stock should be false")
	}
	// Adjust -4 with ticket link → qty 6, txn.ticket_id echoed.
	r = doReq(t, ts, http.MethodPost, "/v1/spares/"+sp.ID+":adjust",
		`{"delta":-4,"ticket_id":"tk_TICKETUSED0001"}`)
	if r.code != http.StatusOK {
		t.Fatalf("adjust -4: %d %s", r.code, r.body)
	}
	mustJSON(t, r.body, &adj)
	if adj.Qty != 6 {
		t.Errorf("after -4: qty=%d, want 6", adj.Qty)
	}
	if len(adj.Recent) != 1 || adj.Recent[0].TicketID != "tk_TICKETUSED0001" {
		t.Errorf("adj recent txn = %+v, want ticket_id=tk_TICKETUSED0001", adj.Recent)
	}
	// GET single → recent_txns lists the most recent movement first.
	r = doReq(t, ts, http.MethodGet, "/v1/spares/"+sp.ID, "")
	var final spareWithDerived
	mustJSON(t, r.body, &final)
	if len(final.Recent) != 2 {
		t.Fatalf("recent len = %d, want 2", len(final.Recent))
	}
	if final.Recent[0].Delta != -4 {
		t.Errorf("newest recent delta = %d, want -4", final.Recent[0].Delta)
	}
	if !final.LowStock && final.Qty < final.MinQty {
		t.Errorf("qty=%d min_qty=%d: low_stock should be true", final.Qty, final.MinQty)
	}
}

func TestSpareHTTP_AdjustRejectBelowZero(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodPost, "/v1/spares",
		`{"sku":"SKU-NEG","name":"x","qty":1,"min_qty":0}`)
	if r.code != http.StatusCreated {
		t.Fatalf("POST: %d %s", r.code, r.body)
	}
	var sp SparePart
	mustJSON(t, r.body, &sp)
	r = doReq(t, ts, http.MethodPost, "/v1/spares/"+sp.ID+":adjust",
		`{"delta":-5}`)
	if r.code != http.StatusUnprocessableEntity {
		t.Fatalf("adjust -5: code=%d body=%s, want 422", r.code, r.body)
	}
	mustProblem(t, r.body, "insufficient-stock")
}

func TestSpareHTTP_AdjustRejectZeroDelta(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodPost, "/v1/spares",
		`{"sku":"SKU-Z","name":"x","qty":3,"min_qty":0}`)
	if r.code != http.StatusCreated {
		t.Fatalf("POST: %d %s", r.code, r.body)
	}
	var sp SparePart
	mustJSON(t, r.body, &sp)
	r = doReq(t, ts, http.MethodPost, "/v1/spares/"+sp.ID+":adjust",
		`{"delta":0}`)
	if r.code != http.StatusBadRequest {
		t.Errorf("adjust 0: code=%d, want 400", r.code)
	}
}

func TestSpareHTTP_LowStockDerived(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodPost, "/v1/spares",
		`{"sku":"SKU-LOW","name":"x","qty":1,"min_qty":5}`)
	if r.code != http.StatusCreated {
		t.Fatalf("POST: %d %s", r.code, r.body)
	}
	var sp SparePart
	mustJSON(t, r.body, &sp)
	r = doReq(t, ts, http.MethodGet, "/v1/spares/"+sp.ID, "")
	var got spareWithDerived
	mustJSON(t, r.body, &got)
	if !got.LowStock {
		t.Errorf("qty=1 min_qty=5: low_stock should be true")
	}
}

func TestSpareHTTP_ListAndPagination(t *testing.T) {
	_, ts := newTestServer(t)
	for i := 0; i < 3; i++ {
		body := `{"sku":"SKU-LIST-` + string(rune('A'+i)) + `","name":"x","qty":1,"min_qty":0}`
		if r := doReq(t, ts, http.MethodPost, "/v1/spares", body); r.code != http.StatusCreated {
			t.Fatalf("seed %d: %d %s", i, r.code, r.body)
		}
	}
	r := doReq(t, ts, http.MethodGet, "/v1/spares?page_size=2", "")
	if r.code != http.StatusOK {
		t.Fatalf("LIST: %d %s", r.code, r.body)
	}
	var list spareListResp
	mustJSON(t, r.body, &list)
	if len(list.Items) != 2 {
		t.Errorf("page 1 len = %d, want 2", len(list.Items))
	}
	if list.NextPageToken == "" {
		t.Errorf("page 1 should have next_page_token")
	}
}

func TestSpareHTTP_DuplicateSKURejected(t *testing.T) {
	// PRMT-080: fileStore now mirrors pgStore — a duplicate SKU
	// on a different id yields ErrSKUExists (422 "sku-exists"),
	// matching the SQL UNIQUE constraint. The first POST mints
	// an id; the second POST mints a DIFFERENT id (id is server-
	// generated, not in the body) but reuses the sku — the second
	// call must be rejected with 422 + problem "sku-exists".
	_, ts := newTestServer(t)
	body := `{"sku":"DUP-1","name":"x","qty":1,"min_qty":0}`
	r := doReq(t, ts, http.MethodPost, "/v1/spares", body)
	if r.code != http.StatusCreated {
		t.Fatalf("first: %d %s", r.code, r.body)
	}
	r = doReq(t, ts, http.MethodPost, "/v1/spares", body)
	if r.code != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate sku: code=%d body=%s, want 422", r.code, r.body)
	}
	mustProblem(t, r.body, "sku-exists")
}

func TestSpareHTTP_DirectQtyPUTNotPossible(t *testing.T) {
	// Spec MUST NOT: there is no PUT /v1/spares/{id} body to
	// change qty directly. Verify any attempt to do so returns
	// 405 (only POST :adjust or GET are valid on /{id}).
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodPost, "/v1/spares",
		`{"sku":"SKU-NOPUT","name":"x","qty":1,"min_qty":0}`)
	if r.code != http.StatusCreated {
		t.Fatalf("POST: %d %s", r.code, r.body)
	}
	var sp SparePart
	mustJSON(t, r.body, &sp)
	// PUT /v1/spares/{id} → 405 (no PUT handler).
	r = doReq(t, ts, http.MethodPut, "/v1/spares/"+sp.ID,
		`{"qty":99}`)
	if r.code != http.StatusMethodNotAllowed {
		t.Errorf("PUT /v1/spares/{id}: code=%d, want 405", r.code)
	}
	// qty must NOT have changed.
	r = doReq(t, ts, http.MethodGet, "/v1/spares/"+sp.ID, "")
	var got spareWithDerived
	mustJSON(t, r.body, &got)
	if got.Qty != 1 {
		t.Errorf("qty drifted to %d via PUT; want 1 (no direct-mutation path)", got.Qty)
	}
}

func TestSpareHTTP_BadIDRejected(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodGet, "/v1/spares/tk_AAAAAAAAAAAAAAAA", "")
	if r.code != http.StatusBadRequest {
		t.Errorf("ticket id shape on /v1/spares/: code=%d, want 400", r.code)
	}
	r = doReq(t, ts, http.MethodGet, "/v1/spares/sp_bogus", "")
	if r.code != http.StatusBadRequest {
		t.Errorf("malformed id: code=%d, want 400", r.code)
	}
}

// --- Auth: 401 when no bearer (PRMT-037 regression guard) -----------

func TestSpareHTTP_NoBearer_401(t *testing.T) {
	// Build a server with auth ENABLED (verifier accepts a known
	// token) so the middleware actually runs and rejects the
	// request. Without a bearer, /v1/spares → 401.
	v, _, _, _ := buildR2Verifier(t, []string{"**"}, []string{"**"})
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	// All four endpoints — list / create / get / :adjust — must
	// 401 when no Bearer is present. The 401 contract lives in
	// the middleware; this test pins the route registration in
	// authmw.go (PRMT-037 lesson — missing endpoint = silent
	// bypass).
	// GET /v1/spares
	r := doReq(t, ts, http.MethodGet, "/v1/spares", "")
	if r.code != http.StatusUnauthorized {
		t.Errorf("GET /v1/spares no-bearer: code=%d, want 401", r.code)
	}
	// POST /v1/spares
	r = doReq(t, ts, http.MethodPost, "/v1/spares",
		`{"sku":"x","name":"y","qty":1,"min_qty":0}`)
	if r.code != http.StatusUnauthorized {
		t.Errorf("POST /v1/spares no-bearer: code=%d, want 401", r.code)
	}
	// POST /v1/spares/{id}:adjust
	r = doReq(t, ts, http.MethodPost, "/v1/spares/sp_AAAAAAAAAAAAAAAA:adjust",
		`{"delta":1}`)
	if r.code != http.StatusUnauthorized {
		t.Errorf("POST ...:adjust no-bearer: code=%d, want 401", r.code)
	}
	// GET /v1/spares/{id}
	r = doReq(t, ts, http.MethodGet, "/v1/spares/sp_AAAAAAAAAAAAAAAA", "")
	if r.code != http.StatusUnauthorized {
		t.Errorf("GET /v1/spares/{id} no-bearer: code=%d, want 401", r.code)
	}
}

// --- Auth: operator can adjust; viewer can list/get -------------------

func TestSpareHTTP_Auth_OperatorAdjustAllowed(t *testing.T) {
	v, _, operatorTok, _ := buildR2Verifier(t, nil, []string{"**"})
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	// Seed one spare directly via the store (bypass auth).
	sp := SparePart{ID: "sp_OPERTEST77777777", SKU: "OP-1", Name: "x", Qty: 5, MinQty: 0}
	if err := srv.st.PutSpare(context.Background(), sp); err != nil {
		t.Fatal(err)
	}
	// Operator: GET /v1/spares → 200.
	r := doReqWithAuth(t, ts, http.MethodGet, "/v1/spares", "", operatorTok)
	if r.code != http.StatusOK {
		t.Errorf("operator list: code=%d body=%s", r.code, r.body)
	}
	// Operator: adjust → 200.
	r = doReqWithAuth(t, ts, http.MethodPost, "/v1/spares/"+sp.ID+":adjust",
		`{"delta":1}`, operatorTok)
	if r.code != http.StatusOK {
		t.Errorf("operator adjust: code=%d body=%s", r.code, r.body)
	}
}

func TestSpareHTTP_Auth_ViewerCannotAdjust(t *testing.T) {
	v, viewerTok, _, _ := buildR2Verifier(t, []string{"**"}, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	sp := SparePart{ID: "sp_VIEWERBLOCKEDZZ", SKU: "V-1", Name: "x", Qty: 5, MinQty: 0}
	if err := srv.st.PutSpare(context.Background(), sp); err != nil {
		t.Fatal(err)
	}
	// Viewer list → 200 (role floor allows).
	r := doReqWithAuth(t, ts, http.MethodGet, "/v1/spares", "", viewerTok)
	if r.code != http.StatusOK {
		t.Errorf("viewer list: code=%d body=%s", r.code, r.body)
	}
	// Viewer adjust → 403 (role floor blocks write).
	r = doReqWithAuth(t, ts, http.MethodPost, "/v1/spares/"+sp.ID+":adjust",
		`{"delta":1}`, viewerTok)
	if r.code != http.StatusForbidden {
		t.Errorf("viewer adjust: code=%d body=%s, want 403", r.code, r.body)
	}
}

// doReqWithAuth is a small helper for the auth-enabled tests;
// server_test.go's doReq does not take a token.
func doReqWithAuth(t *testing.T, ts *httptest.Server, method, path, body, token string) httpResp {
	t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	var req *http.Request
	if rdr != nil {
		req, _ = http.NewRequest(method, ts.URL+path, rdr)
	} else {
		req, _ = http.NewRequest(method, ts.URL+path, nil)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	return httpResp{code: resp.StatusCode, body: string(buf[:n])}
}

// Sanity-check the ID-prefix patterns are anchored (don't match
// empty / partial / longer strings). PRMT-033 R2-1 — same
// boundary check, different prefix.
func TestSpareIDPattern_Anchored(t *testing.T) {
	good := []string{"sp_AAAAAAAAAAAAAAAA", "sp_ZZZZZZZZZZZZZZZZ", "sp_2222222222222222"}
	for _, id := range good {
		if !spareIDPattern.MatchString(id) {
			t.Errorf("expected match: %q", id)
		}
	}
	bad := []string{"", "sp_", "sp_AAAAAAAAAAAAAAAAA", "sp_AAAA", "tk_AAAAAAAAAAAAAAAA", "SP_AAAAAAAAAAAAAAAA"}
	for _, id := range bad {
		if spareIDPattern.MatchString(id) {
			t.Errorf("expected NO match: %q", id)
		}
	}
	// The txn pattern likewise.
	for _, id := range []string{"st_BBBBBBBBBBBBBBBB", "st_2222222222222222"} {
		if !spareTxnIDPattern.MatchString(id) {
			t.Errorf("expected txn match: %q", id)
		}
	}
	for _, id := range []string{"", "st_", "sp_BBBBBBBBBBBBBBBB", "ST_BBBBBBBBBBBBBBBB"} {
		if spareTxnIDPattern.MatchString(id) {
			t.Errorf("expected NO txn match: %q", id)
		}
	}
	// Belt-and-braces: also confirm the regex syntax is anchored.
	matched, _ := regexp.MatchString(`^sp_[A-Z2-7]{16}$`, "sp_AAAAAAAAAAAAAAAA")
	if !matched {
		t.Errorf("regex is wrong")
	}
}

// Guard against the file path being misconfigured — a quick load
// check so test infra failures are surfaced loudly.
func TestSparePG_FixtureIfDSNSet(t *testing.T) {
	if testing.Short() {
		t.Skip("pg integration; skipping in -short mode")
	}
	// pgDSN() t.Skip()s when CIOS_PG_DSN is unset. We exercise
	// AdjustSpare atomicity via the production helper, so the
	// file/pg parity is verified end-to-end through this test.
	env := withPG(t)
	ctx := env.Ctx
	// The pinned connection IS a tx; the migration already ran.
	// Put one spare.
	sp := SparePart{ID: "sp_PGTESTPARITY01", SKU: "PG-SKU", Name: "n", Qty: 4, MinQty: 1}
	if err := putSpare(ctx, env.Conn, sp); err != nil {
		t.Fatalf("putSpare: %v", err)
	}
	// Adjust +3 atomically.
	updated, txn, err := adjustSpare(ctx, env.Conn, sp.ID, 3, "", time.Now().UTC())
	if err != nil {
		t.Fatalf("adjustSpare +3: %v", err)
	}
	if updated.Qty != 7 {
		t.Errorf("after +3 qty=%d, want 7", updated.Qty)
	}
	if txn.Delta != 3 || txn.SpareID != sp.ID {
		t.Errorf("txn = %+v", txn)
	}
	// List txns.
	txns, err := listSpareTxns(ctx, env.Conn, sp.ID)
	if err != nil {
		t.Fatalf("listSpareTxns: %v", err)
	}
	if len(txns) != 1 {
		t.Errorf("txns len = %d, want 1", len(txns))
	}
	// Adjust -99 → ErrInsufficientStock.
	if _, _, err := adjustSpare(ctx, env.Conn, sp.ID, -99, "", time.Now().UTC()); err != ErrInsufficientStock {
		t.Errorf("adjust -99: err=%v, want ErrInsufficientStock", err)
	}
	// SKU duplicate → ErrSKUExists.
	dup := SparePart{ID: "sp_PGTESTPARITY02", SKU: "PG-SKU", Name: "n", Qty: 1, MinQty: 0}
	if err := putSpare(ctx, env.Conn, dup); err != ErrSKUExists {
		t.Errorf("duplicate SKU: err=%v, want ErrSKUExists", err)
	}
}

// --- Spare low-stock scanner (PRMT-054 / M2 E2.5 P541 闭环) -----
//
// Coverage matrix (matches the §7 acceptance regex
// `Spare.*Stock|LowStock`):
//
//   - scanSpareStockTick fires a ticket when Qty<MinQty
//   - scanSpareStockTick does NOT fire when Qty>=MinQty
//   - Dedup: a second tick with the open ticket still present
//     does NOT open a duplicate
//   - Restock to >=MinQty does NOT auto-close (spec-008 Q5)
//   - Dedup key (alarm_id="spare:<id>") namespacing: an open
//     ticket with a different alarm_id (e.g. an alarm-driven
//     ticket on a real asset) does NOT block low-stock firing
//   - RunSpareStockScanner exits cleanly on ctx.Done
//   - Dedup releases after the existing ticket is closed: a
//     fresh low-stock event opens a new ticket

// TestSpareLowStockTick_FiresOnBelowMin plants a spare with
// Qty<MinQty, ticks the scanner, and asserts one open ticket
// was created with the expected shape (severity=minor,
// asset_path="", alarm_id="spare:<id>", title prefix).
func TestSpareLowStockTick_FiresOnBelowMin(t *testing.T) {
	srv, _ := newTestServer(t)
	// Direct store write (bypass auth) so the test is independent
	// of POST /v1/spares body validation.
	sp := SparePart{
		ID: "sp_LOWSTOCK0000001", SKU: "LOW-SKU", Name: "filter",
		Qty: 1, MinQty: 5, Location: "rack-a",
	}
	if err := srv.st.PutSpare(context.Background(), sp); err != nil {
		t.Fatalf("PutSpare: %v", err)
	}
	now := time.Now().UTC()
	srv.scanSpareStockTick(context.Background(), now)

	tickets, err := srv.st.ListTickets(context.Background())
	if err != nil {
		t.Fatalf("ListTickets: %v", err)
	}
	if len(tickets) != 1 {
		t.Fatalf("expected 1 ticket, got %d", len(tickets))
	}
	got := tickets[0]
	if got.Severity != "minor" {
		t.Errorf("Severity = %q, want minor", got.Severity)
	}
	if got.AssetPath != "" {
		t.Errorf("AssetPath = %q, want empty (spares are not asset-path scoped)", got.AssetPath)
	}
	if got.State != "open" {
		t.Errorf("State = %q, want open", got.State)
	}
	wantAlarm := spareAlarmID(sp.ID)
	if got.AlarmID != wantAlarm {
		t.Errorf("AlarmID = %q, want %q (dedup key)", got.AlarmID, wantAlarm)
	}
	if !strings.HasPrefix(got.Title, "Low stock: LOW-SKU") {
		t.Errorf("Title = %q, want prefix 'Low stock: LOW-SKU'", got.Title)
	}
}

// TestSpareLowStockTick_SkipsWhenAboveMin plants a spare with
// Qty>=MinQty, ticks the scanner, and asserts no ticket was
// created.
func TestSpareLowStockTick_SkipsWhenAboveMin(t *testing.T) {
	srv, _ := newTestServer(t)
	sp := SparePart{
		ID: "sp_OKSTOCK00000001", SKU: "OK-SKU", Name: "x",
		Qty: 10, MinQty: 5,
	}
	if err := srv.st.PutSpare(context.Background(), sp); err != nil {
		t.Fatalf("PutSpare: %v", err)
	}
	srv.scanSpareStockTick(context.Background(), time.Now().UTC())
	tickets, _ := srv.st.ListTickets(context.Background())
	if len(tickets) != 0 {
		t.Errorf("above-min spare: expected 0 tickets, got %d", len(tickets))
	}
}

// TestSpareLowStockTick_DedupNoDuplicate ticks twice on the same
// low-stock spare; the second tick must NOT open a duplicate
// ticket. The first tick leaves an open ticket pinned via
// alarm_id="spare:<id>" — that is the dedup gate.
func TestSpareLowStockTick_DedupNoDuplicate(t *testing.T) {
	srv, _ := newTestServer(t)
	sp := SparePart{
		ID: "sp_DEDUPSTOCK00001", SKU: "DEDUP-SKU", Name: "x",
		Qty: 0, MinQty: 2,
	}
	if err := srv.st.PutSpare(context.Background(), sp); err != nil {
		t.Fatalf("PutSpare: %v", err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	srv.scanSpareStockTick(ctx, now)
	srv.scanSpareStockTick(ctx, now.Add(time.Minute))
	srv.scanSpareStockTick(ctx, now.Add(2*time.Minute))

	tickets, _ := srv.st.ListTickets(context.Background())
	if len(tickets) != 1 {
		t.Fatalf("expected 1 ticket (dedup), got %d", len(tickets))
	}
	if tickets[0].AlarmID != spareAlarmID(sp.ID) {
		t.Errorf("dedup key drifted: alarm_id=%q", tickets[0].AlarmID)
	}
}

// TestSpareLowStockTick_RestockDoesNotAutoClose covers spec-008
// Q5: restocking the spare to >=MinQty does NOT auto-close the
// open low-stock ticket. The scanner leaves it for human
// acknowledgement. The dedup gate also stays armed — a second
// tick after restock still skips (because the open ticket is
// still there), but if the ticket is then closed AND qty drops
// again, a fresh ticket opens (covered separately in
// TestSpareLowStockTick_RestocksThenCloses).
func TestSpareLowStockTick_RestockDoesNotAutoClose(t *testing.T) {
	srv, _ := newTestServer(t)
	sp := SparePart{
		ID: "sp_RESTOCKSTOCK001", SKU: "RESTOCK-SKU", Name: "x",
		Qty: 0, MinQty: 3,
	}
	if err := srv.st.PutSpare(context.Background(), sp); err != nil {
		t.Fatalf("PutSpare: %v", err)
	}
	// First tick → open a ticket.
	now := time.Now().UTC()
	srv.scanSpareStockTick(context.Background(), now)
	tickets, _ := srv.st.ListTickets(context.Background())
	if len(tickets) != 1 {
		t.Fatalf("expected 1 ticket after first tick, got %d", len(tickets))
	}
	openTk := tickets[0]
	if openTk.State != "open" {
		t.Fatalf("first ticket state = %q, want open", openTk.State)
	}
	// Restock: qty 0 → 10 (>= MinQty 3). AdjustSpare is the only
	// legal path; the scanner must not need to do anything here.
	if _, _, err := srv.st.AdjustSpare(context.Background(), sp.ID, 10, "", time.Now().UTC()); err != nil {
		t.Fatalf("AdjustSpare +10: %v", err)
	}
	// Scan again — restock does NOT close the ticket.
	srv.scanSpareStockTick(context.Background(), now.Add(time.Minute))
	tickets, _ = srv.st.ListTickets(context.Background())
	if len(tickets) != 1 {
		t.Fatalf("expected 1 ticket after restock scan, got %d", len(tickets))
	}
	if tickets[0].State != "open" {
		t.Errorf("restock must NOT auto-close; state = %q, want open", tickets[0].State)
	}
	if tickets[0].ID != openTk.ID {
		t.Errorf("restock must NOT open a new ticket; id drifted from %q to %q",
			openTk.ID, tickets[0].ID)
	}
}

// TestSpareLowStockTick_RestocksThenCloses covers the
// re-firing-after-acknowledgement case: ticket is manually
// closed, then qty drops again, and the scanner opens a fresh
// ticket. This is the "every low-stock event gets a ticket"
// guarantee — the open ticket is the dedup gate, not a permanent
// suppress.
func TestSpareLowStockTick_RestocksThenCloses(t *testing.T) {
	srv, _ := newTestServer(t)
	sp := SparePart{
		ID: "sp_REFIRELATER001", SKU: "REFIRE-SKU", Name: "x",
		Qty: 0, MinQty: 2,
	}
	if err := srv.st.PutSpare(context.Background(), sp); err != nil {
		t.Fatalf("PutSpare: %v", err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	// First low-stock event → open ticket.
	srv.scanSpareStockTick(ctx, now)
	tickets, _ := srv.st.ListTickets(context.Background())
	if len(tickets) != 1 {
		t.Fatalf("first tick: expected 1 ticket, got %d", len(tickets))
	}
	first := tickets[0]
	// Restock → qty above min (operator action).
	if _, _, err := srv.st.AdjustSpare(context.Background(), sp.ID, 10, "", time.Now().UTC()); err != nil {
		t.Fatalf("AdjustSpare +10: %v", err)
	}
	// Operator closes the ticket (manual ack → close).
	first.State = "closed"
	closedAt := time.Now().UTC()
	first.ClosedAt = &closedAt
	if _, err := srv.st.PutTicket(context.Background(), first, 0); err != nil {
		t.Fatalf("close ticket: %v", err)
	}
	// qty drops below min again (consumption → ticket-driven).
	if _, _, err := srv.st.AdjustSpare(context.Background(), sp.ID, -9, "", time.Now().UTC()); err != nil {
		t.Fatalf("AdjustSpare -9: %v", err)
	}
	// Scanner ticks → fresh ticket.
	srv.scanSpareStockTick(ctx, now.Add(2*time.Minute))
	tickets, _ = srv.st.ListTickets(context.Background())
	if len(tickets) != 2 {
		t.Fatalf("expected 2 tickets (first closed + re-fire), got %d", len(tickets))
	}
	// Find the open one; it must be a NEW id, not the closed one.
	var openCount, closedCount int
	for _, t2 := range tickets {
		if t2.State == "open" {
			openCount++
			if t2.ID == first.ID {
				t.Errorf("re-fire must mint a new id; got the closed ticket id back")
			}
			if t2.AlarmID != spareAlarmID(sp.ID) {
				t.Errorf("re-fire alarm_id = %q, want %q", t2.AlarmID, spareAlarmID(sp.ID))
			}
		} else {
			closedCount++
		}
	}
	if openCount != 1 {
		t.Errorf("expected 1 open ticket after re-fire, got %d", openCount)
	}
	if closedCount != 1 {
		t.Errorf("expected 1 closed ticket after re-fire, got %d", closedCount)
	}
}

// TestSpareLowStockTick_DedupIgnoresForeignAlarmID asserts the
// dedup key is namespaced: an existing open ticket with a
// DIFFERENT alarm_id (e.g. an alarm-driven ticket on a real
// asset path) must NOT block low-stock firing. This pins the
// "spare:" namespace convention.
func TestSpareLowStockTick_DedupIgnoresForeignAlarmID(t *testing.T) {
	srv, _ := newTestServer(t)
	sp := SparePart{
		ID: "sp_FOREIGNALARM001", SKU: "FGN-SKU", Name: "x",
		Qty: 0, MinQty: 2,
	}
	if err := srv.st.PutSpare(context.Background(), sp); err != nil {
		t.Fatalf("PutSpare: %v", err)
	}
	// Plant an open ticket with a foreign alarm_id (alarm-driven
	// shape). The dedup key MUST ignore this.
	foreign := Ticket{
		ID:        "tk_AAAAAAAAAAAAAAAA",
		AlarmID:   "io.cios.alarm.bogus",
		AssetPath: "sgp01.pod001.cdu000.fws.supply.flow",
		Title:     "alarm-driven ticket",
		Severity:  "major",
		State:     "open",
		OpenedAt:  time.Now().UTC(),
	}
	if _, err := srv.st.PutTicket(context.Background(), foreign, 0); err != nil {
		t.Fatalf("PutTicket foreign: %v", err)
	}
	// Tick the scanner — it must open a fresh low-stock ticket
	// (the foreign one does not match the "spare:<id>" dedup key).
	srv.scanSpareStockTick(context.Background(), time.Now().UTC())
	tickets, _ := srv.st.ListTickets(context.Background())
	if len(tickets) != 2 {
		t.Fatalf("expected 2 tickets (foreign + low-stock), got %d", len(tickets))
	}
	var sawLowStock bool
	for _, t2 := range tickets {
		if t2.AlarmID == spareAlarmID(sp.ID) {
			sawLowStock = true
		}
	}
	if !sawLowStock {
		t.Errorf("low-stock ticket not opened; foreign alarm_id must not block")
	}
}

// TestRunSpareStockScannerExitsOnCtx asserts the long-lived
// scanner returns when its ctx is cancelled (mirrors PM/SLA
// scanner shutdown contract).
func TestRunSpareStockScannerExitsOnCtx(t *testing.T) {
	srv, _ := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		srv.RunSpareStockScanner(ctx, 50*time.Millisecond)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("RunSpareStockScanner did not exit on ctx cancel")
	}
}

// TestSpareLowStockTick_BelowMinZeroAndEqual is a tiny boundary
// case: qty=min_qty is NOT low (the contract is strict less-than,
// mirroring the derived low_stock flag in serveSpareGet). The
// store comparison `sp.Qty < sp.MinQty` is exclusive by design.
func TestSpareLowStockTick_BelowMinZeroAndEqual(t *testing.T) {
	srv, _ := newTestServer(t)
	cases := []struct {
		name        string
		qty, minQty int
		wantLow     bool
	}{
		{"below", 1, 5, true},
		{"equal", 5, 5, false},
		{"above", 6, 5, false},
		{"zero", 0, 1, true},
	}
	for i, tc := range cases {
		sp := SparePart{
			ID:     fmt.Sprintf("sp_BOUNDARYTEST%04d", i),
			SKU:    fmt.Sprintf("BND-%d", i),
			Name:   "x",
			Qty:    tc.qty,
			MinQty: tc.minQty,
		}
		if err := srv.st.PutSpare(context.Background(), sp); err != nil {
			t.Fatalf("PutSpare %s: %v", tc.name, err)
		}
	}
	srv.scanSpareStockTick(context.Background(), time.Now().UTC())
	tickets, _ := srv.st.ListTickets(context.Background())
	var opened int
	for _, t2 := range tickets {
		_ = t2
		opened++
	}
	wantOpened := 0
	for _, tc := range cases {
		if tc.wantLow {
			wantOpened++
		}
	}
	if opened != wantOpened {
		t.Errorf("opened %d tickets, want %d (below/equal/above/zero → expect strict-less)",
			opened, wantOpened)
	}
}
