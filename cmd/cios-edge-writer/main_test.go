package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/yurimeng/cios/pkg/natspub"
)

// --- DATA-RESILIENCE G1: transport failures use NakWithDelay, never
//     Ack-drop as "poison" (even at high delivery counts).

func TestEdgeWriter_TransientNakNeverPoisonDrop(t *testing.T) {
	setup := func(t *testing.T, reason string) (*http.Client, string) {
		t.Helper()
		switch reason {
		case "build-POST-failed":
			return &http.Client{Timeout: 2 * time.Second}, "http://example.com/\nbad"
		case "POST-network-failed":
			return &http.Client{Timeout: 500 * time.Millisecond}, "http://127.0.0.1:1/"
		case "POST-non-2xx":
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			}))
			t.Cleanup(srv.Close)
			return &http.Client{Timeout: 2 * time.Second}, srv.URL
		default:
			t.Fatalf("unknown reason %q", reason)
			return nil, ""
		}
	}

	drive := func(t *testing.T, hc *http.Client, vmURL string, dc int) string {
		t.Helper()
		var buf bytes.Buffer
		old := log.Writer()
		log.SetOutput(&buf)
		t.Cleanup(func() { log.SetOutput(old) })

		handler := makeHandler(context.Background(), hc, vmURL, "")
		batch := natspub.TelemetryBatch{
			Encoding: "promtext",
			Site:     "sgp01",
			Lines:    []string{`metric{path="x.y.z"} 1.0`},
		}
		data, _ := json.Marshal(batch)
		msg := &nats.Msg{
			Subject: "cios.tlm.sgp01.cdu000",
			Data:    data,
			Reply:   jsmAckReply(uint(dc)),
			Sub:     &nats.Subscription{},
		}
		handler(msg)
		return buf.String()
	}

	// High delivery counts used to Ack-drop; G1 keeps nak-delay forever.
	for _, dc := range []int{1, 5, 50} {
		for _, reason := range []string{"build-POST-failed", "POST-network-failed", "POST-non-2xx"} {
			dc, reason := dc, reason
			t.Run(reason+"_dc"+strconv.Itoa(dc), func(t *testing.T) {
				hc, vmURL := setup(t, reason)
				out := drive(t, hc, vmURL, dc)
				if strings.Contains(out, "dropping poison message") {
					t.Fatalf("transport path must not poison-drop: %s", out)
				}
				if !strings.Contains(out, "nak-delay") && !strings.Contains(out, "naking") {
					// non-2xx logs "nak-delay"; network may log "nak-delay" too
					if !strings.Contains(out, "deliveries=") {
						t.Fatalf("want nak-delay/deliveries log: %s", out)
					}
				}
			})
		}
	}
}

// --- PRMT-030 §B.5: postBatchToVM four paths ---------------------------
//
// postBatchToVM is the IO helper extracted from the prior 74-line
// handler closure. The four paths below pin its observable contract:
//   - happy: 2xx → (status, nil)
//   - build-fail: URL that http.NewRequestWithContext rejects → (0, errBuildPost)
//   - Do-fail: server unreachable → (0, errDoPost)
//   - non-2xx: server returns 500 → (500, nil)
//
// The build-fail path uses a URL containing a control byte that
// http.NewRequestWithContext rejects ("\n" in the URL). The Do-fail
// path uses an http.Client whose Transport is closed so Do returns
// immediately. The non-2xx path uses httptest.NewServer returning 500.

func TestPostBatchToVM_Happy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method=%s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "text/plain" {
			t.Errorf("Content-Type=%q, want text/plain", ct)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hc := &http.Client{Timeout: 2 * time.Second}
	status, err := postBatchToVM(context.Background(), hc, srv.URL, "metric 1\nmetric 2")
	if err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status=%d, want %d", status, http.StatusOK)
	}
}

func TestPostBatchToVM_BuildFail(t *testing.T) {
	// "\n" makes http.NewRequestWithContext reject the URL.
	hc := &http.Client{Timeout: 2 * time.Second}
	status, err := postBatchToVM(context.Background(), hc, "http://example.com/\nbad", "body")
	if err == nil {
		t.Fatalf("err=nil, want build-fail; status=%d", status)
	}
	if status != 0 {
		t.Errorf("status=%d, want 0 on build-fail", status)
	}
	if !errors.Is(err, errBuildPost) {
		t.Errorf("err=%v, want errors.Is(err, errBuildPost)", err)
	}
	if errors.Is(err, errDoPost) {
		t.Errorf("err=%v, must NOT match errDoPost on build-fail", err)
	}
}

func TestPostBatchToVM_DoFail(t *testing.T) {
	// Point at a port nothing is listening on; Do fails immediately.
	hc := &http.Client{Timeout: 500 * time.Millisecond}
	status, err := postBatchToVM(context.Background(), hc, "http://127.0.0.1:1/", "body")
	if err == nil {
		t.Fatalf("err=nil, want Do-fail; status=%d", status)
	}
	if status != 0 {
		t.Errorf("status=%d, want 0 on Do-fail", status)
	}
	if !errors.Is(err, errDoPost) {
		t.Errorf("err=%v, want errors.Is(err, errDoPost)", err)
	}
}

func TestPostBatchToVM_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	hc := &http.Client{Timeout: 2 * time.Second}
	status, err := postBatchToVM(context.Background(), hc, srv.URL, "body")
	if err != nil {
		t.Fatalf("err=%v, want nil on non-2xx (caller inspects status)", err)
	}
	if status != http.StatusInternalServerError {
		t.Errorf("status=%d, want %d", status, http.StatusInternalServerError)
	}
}

// --- PRMT-030 §B.5 R6 必办 1.1: next-message after error ---------------
//
// Two-call sequence on the production handler (package-level
// makeHandler, R4 A 路):
//  1. nil msg.Data → production handler's first non-trivial call
//     json.Unmarshal(msg.Data, &batch) returns an error
//     ("unexpected end of JSON input", NOT a panic). The handler
//     logs "decode: ... (acking)" and Acks. This is the
//     architect-approved error path for the first call
//     (R6 裁定 1 / B 路: json.Unmarshal error / postBatchToVM 5xx).
//  2. A valid promtext batch that flows through postBatchToVM into
//     a httptest stub VM. vmHits > 0 proves the second message
//     reached postBatchToVM after the first one — i.e. the handler
//     returns normally between calls and the subscription would
//     continue processing subsequent messages. This is the
//     "next message is still processed" contract in real runtime.
//
// Production handler's defer-recover panic safety is statically
// verified by §B.7 (R6 裁定 1 增项): the handler's first defer
// MUST be the recover() block, before any Ack/Nak. Per R6 B 路
// decision, runtime panic-injection is NOT used here (would
// require a panic-flag in production, which is out of scope).
//
// This test name satisfies §B.5 MUST B-路 phrasing: "次条消息在
// 前条出错后仍被处理 (经 makeHandler 真实驱动)".
func TestEdgeWriterHandler_NextMessageAfterError(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	var vmHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&vmHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hc := &http.Client{Timeout: 2 * time.Second}
	handler := makeHandler(context.Background(), hc, srv.URL, "")

	// First call: nil msg.Data. Production handler returns an
	// "unexpected end of JSON input" error, logs, and Acks.
	nilMsg := &nats.Msg{Subject: "cios.tlm.sgp01.cdu000", Sub: &nats.Subscription{}}
	nilMsg.Data = nil
	handler(nilMsg)

	// Second call: a valid promtext batch that flows through
	// postBatchToVM into the httptest VM. vmHits > 0 proves the
	// handler survived the first call.
	batch := natspub.TelemetryBatch{
		Encoding: "promtext",
		Site:     "sgp01",
		Lines:    []string{`metric{path="x.y.z"} 1.0`},
	}
	data, _ := json.Marshal(batch)
	valid := &nats.Msg{
		Subject: "cios.tlm.sgp01.cdu000",
		Data:    data,
		Sub:     &nats.Subscription{},
	}
	handler(valid)
	if got := atomic.LoadInt32(&vmHits); got == 0 {
		t.Errorf("vmHits=0, expected >=1 (handler must reach postBatchToVM on the second call)")
	}
}

// jsmAckReply builds a v1-format $JS.ACK subject carrying dc as the
// NumDelivered token. Format per nats-io/internal/parser:
//
//	$JS.ACK.<stream>.<consumer>.<delivered>.<sseq>.<cseq>.<tm>.<pending>
//
// Sub must be non-nil for nats.Msg.checkReply to succeed; the
// caller hands makeHandler a zero-value Subscription, and Metadata()
// reads only the Reply subject, not the Sub fields.
func jsmAckReply(dc uint) string {
	return "$JS.ACK.s.c." + strconv.FormatUint(uint64(dc), 10) + ".1.1.1234.0"
}
