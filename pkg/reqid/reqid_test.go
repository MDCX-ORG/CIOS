// PRMT-030 §A.5 — verify the request_id shape stays "01H" prefix,
// 19-char total length, base32 (no padding) charset.
package reqid

import (
	"encoding/base32"
	"strings"
	"testing"
)

func TestNew_PrefixAndLength(t *testing.T) {
	id := New()
	if !strings.HasPrefix(id, "01H") {
		t.Fatalf("id=%q, want 01H prefix", id)
	}
	if len(id) != 3+16 {
		t.Fatalf("len(id)=%d, want 19", len(id))
	}
}

func TestNew_Charset(t *testing.T) {
	// base32.StdEncoding alphabet is A-Z 2-7. The encoded body must
	// contain only those chars (no '=' — we TrimRight above).
	enc := base32.StdEncoding.WithPadding(base32.NoPadding)
	alphabet := enc.EncodeToString(make([]byte, 0)) // empty string → ""
	if alphabet != "" {
		t.Fatalf("unexpected alphabet baseline %q", alphabet)
	}
	for i := 0; i < 50; i++ {
		id := New()
		body := id[3:]
		for _, r := range body {
			ok := (r >= 'A' && r <= 'Z') || (r >= '2' && r <= '7')
			if !ok {
				t.Fatalf("id=%q has bad char %q", id, r)
			}
		}
	}
}

func TestNew_Uniqueness(t *testing.T) {
	// 10 bytes = 80 bits of entropy. 100 draws must all differ.
	seen := map[string]struct{}{}
	for i := 0; i < 100; i++ {
		id := New()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %q at i=%d", id, i)
		}
		seen[id] = struct{}{}
	}
}

func TestNew_Stable(t *testing.T) {
	// Two consecutive calls differ (proves rand is wired, not a
	// constant return).
	a, b := New(), New()
	if a == b {
		t.Fatalf("two consecutive New() calls collided: %q", a)
	}
}
