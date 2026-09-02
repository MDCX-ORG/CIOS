package resilmetrics

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestWriteCounterAndLabeled(t *testing.T) {
	var buf bytes.Buffer
	var c Counter
	c.Add(3)
	WriteCounter(&buf, "cios_test_total", "help", c.Get())
	if !strings.Contains(buf.String(), "cios_test_total 3") {
		t.Fatalf("%s", buf.String())
	}
	var lc LabeledCounter
	lc.With("decode").Inc()
	lc.With("encoding").Add(2)
	buf.Reset()
	WriteLabeledCounter(&buf, "cios_test_drops_total", "drops", "reason", &lc)
	out := buf.String()
	if !strings.Contains(out, `reason="decode"`) || !strings.Contains(out, `reason="encoding"`) {
		t.Fatalf("%s", out)
	}
}

func TestListenAndScrape(t *testing.T) {
	var c Counter
	c.Inc()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	stop, err := Listen(addr, func(w io.Writer) {
		WriteCounter(w, "cios_scrape_total", "n", c.Get())
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	deadline := time.Now().Add(2 * time.Second)
	var body []byte
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/metrics")
		if err == nil {
			body, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			if strings.Contains(string(body), "cios_scrape_total 1") {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("scrape failed body=%s", body)
}
