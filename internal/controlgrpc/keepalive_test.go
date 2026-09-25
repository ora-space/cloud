package controlgrpc

import (
	"net"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// TestKeepalivePingsOnIdleConnectionNeverTriggerGoaway pins the listener's PING enforcement policy
// with a raw HTTP/2 client, because a gRPC client hides the PING traffic and the GOAWAY reason.
// grpc-go does not count the first PING and sends GOAWAY(too_many_pings) once more than two later
// PINGs violate the policy, so four PINGs spaced just over keepaliveMinTime on a connection without
// streams fail on the fourth under the default policy and must all be acknowledged under ours.
// Spec: specs/test-cases/cloud/controller-integration/cloud-owned-internal-grpc-contract.md#controller-keepalive-pings-never-trigger-goaway
func TestKeepalivePingsOnIdleConnectionNeverTriggerGoaway(t *testing.T) {
	t.Parallel()
	const pings = 4
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatalf("listen: %v", e)
	}
	// No RPC is ever sent, so the server never reaches the store it was built with.
	server := New(nil)
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		if e := <-served; e != nil {
			t.Errorf("serve: %v", e)
		}
	})

	conn, e := net.Dial("tcp", listener.Addr().String())
	if e != nil {
		t.Fatalf("dial: %v", e)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, e := conn.Write([]byte(http2.ClientPreface)); e != nil {
		t.Fatalf("write preface: %v", e)
	}
	framer := http2.NewFramer(conn, conn)
	if e := framer.WriteSettings(); e != nil {
		t.Fatalf("write settings: %v", e)
	}

	// The reader owns every framer read and the SETTINGS acknowledgements; the test goroutine only
	// writes PINGs. Framer allows one concurrent reader and one concurrent writer, and the reader's
	// single write (the SETTINGS ACK) precedes the first PING because the test waits for it.
	type frameEvent struct {
		settled bool
		pingAck bool
		goAway  *http2.GoAwayFrame
		err     error
	}
	events := make(chan frameEvent, 16)
	go func() {
		for {
			frame, e := framer.ReadFrame()
			if e != nil {
				events <- frameEvent{err: e}
				return
			}
			switch f := frame.(type) {
			case *http2.SettingsFrame:
				if !f.IsAck() {
					if e := framer.WriteSettingsAck(); e != nil {
						events <- frameEvent{err: e}
						return
					}
					events <- frameEvent{settled: true}
				}
			case *http2.PingFrame:
				if f.IsAck() {
					events <- frameEvent{pingAck: true}
				}
			case *http2.GoAwayFrame:
				events <- frameEvent{goAway: f}
				return
			}
		}
	}()

	// next returns the following event or fails the test when the server stays silent.
	next := func() frameEvent {
		t.Helper()
		select {
		case event := <-events:
			return event
		case <-time.After(5 * time.Second):
			t.Fatal("no frame from the server within 5s")
			return frameEvent{}
		}
	}
	for event := next(); !event.settled; event = next() {
		if event.err != nil || event.goAway != nil {
			t.Fatalf("connection ended before settings: %+v", event)
		}
	}

	acked := 0
	for i := range pings {
		if i > 0 {
			time.Sleep(keepaliveMinTime + 500*time.Millisecond)
		}
		if e := framer.WritePing(false, [8]byte{byte(i)}); e != nil {
			t.Fatalf("ping %d: %v", i+1, e)
		}
		event := next()
		if event.goAway != nil {
			t.Fatalf("ping %d drew GOAWAY %v %q after %d acknowledged pings", i+1, event.goAway.ErrCode, event.goAway.DebugData(), acked)
		}
		if event.err != nil || !event.pingAck {
			t.Fatalf("ping %d: expected PING ACK, got %+v", i+1, event)
		}
		acked++
	}
	if acked != pings {
		t.Fatalf("expected %d PING ACKs, got %d", pings, acked)
	}
	// The server acknowledges a PING before judging it, so a GOAWAY for the last PING would follow its
	// ACK. An immediate probe PING flushes it out: the server writes control frames in order, so a
	// pending GOAWAY arrives before the probe's ACK. The probe itself is only a first strike under
	// our policy.
	if e := framer.WritePing(false, [8]byte{0xff}); e != nil {
		t.Fatalf("probe ping: %v", e)
	}
	event := next()
	if event.goAway != nil {
		t.Fatalf("GOAWAY %v %q after %d acknowledged pings", event.goAway.ErrCode, event.goAway.DebugData(), acked)
	}
	if event.err != nil || !event.pingAck {
		t.Fatalf("probe ping: expected PING ACK, got %+v", event)
	}
}
