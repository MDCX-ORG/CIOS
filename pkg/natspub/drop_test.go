// Tests for DropIfPoison, NakBackoff, DeliveryCount (DATA-RESILIENCE G1).
package natspub

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// jsmAckReply builds a v1-format $JS.ACK subject carrying dc as the
// NumDelivered token.
func jsmAckReply(dc uint) string {
	return "$JS.ACK.s.c." +
		itoa(uint64(dc)) + ".1.1.1234.0"
}

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func TestNakBackoff_Ladder(t *testing.T) {
	if NakBackoff(1) != 5*time.Second {
		t.Fatal(NakBackoff(1))
	}
	if NakBackoff(2) != 15*time.Second {
		t.Fatal(NakBackoff(2))
	}
	if NakBackoff(3) != 30*time.Second {
		t.Fatal(NakBackoff(3))
	}
	if NakBackoff(4) != time.Minute {
		t.Fatal(NakBackoff(4))
	}
	if NakBackoff(5) != 2*time.Minute || NakBackoff(99) != 2*time.Minute {
		t.Fatal("cap 2m")
	}
}

func TestDeliveryCount(t *testing.T) {
	m := &nats.Msg{
		Subject: "cios.tlm.sgp01.pod002",
		Reply:   jsmAckReply(3),
		Sub:     &nats.Subscription{},
	}
	if DeliveryCount(m) != 3 {
		t.Fatalf("got %d", DeliveryCount(m))
	}
	if DeliveryCount(&nats.Msg{}) != 1 {
		t.Fatal("no meta → 1")
	}
}

func TestDropIfPoison_AtCap(t *testing.T) {
	m := &nats.Msg{
		Subject: "cios.tlm.sgp01.pod002",
		Reply:   jsmAckReply(PoisonDeliverCap),
		Sub:     &nats.Subscription{},
	}
	drop, dc := DropIfPoison(m)
	if !drop || dc != PoisonDeliverCap {
		t.Fatalf("drop=%v dc=%d", drop, dc)
	}
}

func TestDropIfPoison_BelowCap(t *testing.T) {
	m := &nats.Msg{
		Subject: "cios.tlm.sgp01.pod002",
		Reply:   jsmAckReply(1),
		Sub:     &nats.Subscription{},
	}
	drop, dc := DropIfPoison(m)
	if drop || dc != 1 {
		t.Fatalf("drop=%v dc=%d", drop, dc)
	}
}

func TestDropIfPoison_NoMetadata(t *testing.T) {
	m := &nats.Msg{Subject: "cios.tlm.sgp01.pod002", Sub: &nats.Subscription{}}
	drop, dc := DropIfPoison(m)
	if drop || dc != 0 {
		t.Fatalf("drop=%v dc=%d", drop, dc)
	}
}

func TestDropIfPoison_NilMsg(t *testing.T) {
	drop, dc := DropIfPoison(nil)
	if drop || dc != 0 {
		t.Fatalf("nil: drop=%v dc=%d", drop, dc)
	}
}

func TestTransientMaxDeliverUnlimited(t *testing.T) {
	if TransientMaxDeliver != -1 {
		t.Fatalf("want -1 unlimited, got %d", TransientMaxDeliver)
	}
}
