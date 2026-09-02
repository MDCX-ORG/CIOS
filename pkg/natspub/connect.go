// Package natspub — connect.go: durable NATS connect options (DATA-RESILIENCE G2).
//
// Default nats.Connect exhausts MaxReconnects≈60 (~2 min) then permanently
// closes — gateway WAL fills and edge-writer becomes a zombie consumer.
// These options reconnect forever and log disconnect/reconnect.
package natspub

import (
	"log"
	"time"

	"github.com/nats-io/nats.go"
)

// ConnectOpts returns options for long-lived telemetry daemons.
// closedHandler is invoked when the connection is fully closed (after
// Drain or irrecoverable close). Callers that want "die loudly" on
// unexpected close should set intentionalDrain and Exit in that handler.
func ConnectOpts(name string, closedHandler nats.ConnHandler) []nats.Option {
	if name == "" {
		name = "cios"
	}
	opts := []nats.Option{
		nats.Name(name),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2 * time.Second),
		nats.RetryOnFailedConnect(true),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				log.Printf("%s: nats disconnected: %v", name, err)
			} else {
				log.Printf("%s: nats disconnected", name)
			}
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			log.Printf("%s: nats reconnected to %s", name, nc.ConnectedUrl())
		}),
	}
	if closedHandler != nil {
		opts = append(opts, nats.ClosedHandler(closedHandler))
	}
	return opts
}
