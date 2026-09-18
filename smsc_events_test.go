package main

import (
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/getsyntegrity/go-specs/specs"
)

const BIND_TRANSCEIVER_RESP = 0x80000009

// recordingConn is a net.Conn that treats every Write as one indivisible PDU. It holds the
// write open for a moment so two concurrent writers really do collide, and reports whether any
// two writes were ever in flight at the same time. On a real socket that overlap is exactly
// what corrupts the SMPP stream.
type recordingConn struct {
	net.Conn
	mu         sync.Mutex
	inFlight   int
	overlapped bool
	writes     [][]byte
}

func (c *recordingConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.inFlight++
	if c.inFlight > 1 {
		c.overlapped = true
	}
	c.mu.Unlock()

	time.Sleep(time.Millisecond)

	c.mu.Lock()
	written := make([]byte, len(p))
	copy(written, p)
	c.writes = append(c.writes, written)
	c.inFlight--
	c.mu.Unlock()

	return len(p), nil
}

func (c *recordingConn) didOverlap() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.overlapped
}

func (c *recordingConn) written() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]byte, len(c.writes))
	copy(out, c.writes)
	return out
}

func writeConcurrently(conn net.Conn, writers int, pdu []byte) {
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func() {
			defer wg.Done()
			conn.Write(pdu)
		}()
	}
	wg.Wait()
}

// eventCollector is the TUI's stand-in: it subscribes to the sink and is read from the test
// goroutine while the connection goroutine keeps emitting.
type eventCollector struct {
	mu     sync.Mutex
	events []Event
}

func (c *eventCollector) record(event Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
}

func (c *eventCollector) snapshot() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Event, len(c.events))
	copy(out, c.events)
	return out
}

// the connection goroutine emits on its own schedule, so the expected event is polled
func (c *eventCollector) await(kind EventKind, commandId uint32) (Event, bool) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, event := range c.snapshot() {
			if event.Kind != kind {
				continue
			}
			if commandId != 0 && event.CommandId != commandId {
				continue
			}
			return event, true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return Event{}, false
}

func startSmsc(t *testing.T, smsc *Smsc) net.Conn {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	go smsc.serve(listener)

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("cannot dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	return conn
}

func TestConnectionWritesAreSerialized(t *testing.T) {
	specs.Describe(t, "writes to a single SMPP connection", func(s *specs.Spec) {
		s.When("the raw connection is written to by several goroutines at once", func(s *specs.Spec) {
			s.It("lets two PDUs overlap, which is the corruption this wrapper exists to stop", func(ctx *specs.Context) {
				raw := &recordingConn{}

				writeConcurrently(raw, 8, headerPDU(ENQUIRE_LINK_RESP, STS_OK, 1))

				ctx.Expect(raw.didOverlap()).To(specs.BeTrue())
			})
		})

		s.When("the same goroutines write through the serialized connection", func(s *specs.Spec) {
			s.It("never lets one PDU begin before the previous one finished", func(ctx *specs.Context) {
				raw := &recordingConn{}
				conn := newSyncConn(raw)

				writeConcurrently(conn, 8, headerPDU(ENQUIRE_LINK_RESP, STS_OK, 1))

				ctx.Expect(raw.didOverlap()).To(specs.BeFalse())
			})

			s.It("delivers every PDU whole, none truncated and none merged", func(ctx *specs.Context) {
				raw := &recordingConn{}
				conn := newSyncConn(raw)
				pdu := stringBodyPDU(BIND_TRANSCEIVER_RESP, STS_OK, 1, "smscsim")

				writeConcurrently(conn, 8, pdu)

				written := raw.written()
				ctx.Expect(len(written)).ToEqual(8)
				for _, chunk := range written {
					ctx.Expect(len(chunk)).ToEqual(len(pdu))
				}
			})

			s.It("still satisfies net.Conn, so every existing call site keeps compiling", func(ctx *specs.Context) {
				var conn net.Conn = newSyncConn(&recordingConn{})
				ctx.Expect(conn == nil).To(specs.BeFalse())
			})
		})
	})
}

func TestSmscEventSink(t *testing.T) {
	specs.Describe(t, "the Smsc event sink", func(s *specs.Spec) {
		s.When("no sink has been registered", func(s *specs.Spec) {
			s.It("serves a bind and an enquire_link without panicking", func(ctx *specs.Context) {
				smsc := NewSmsc(false)
				conn := startSmsc(ctx.T, smsc)

				writePDU(ctx.T, conn, stringBodyPDU(BIND_TRANSCEIVER, STS_OK, 1, "silent"))
				readPDU(ctx.T, conn)
				writePDU(ctx.T, conn, headerPDU(ENQUIRE_LINK, STS_OK, 2))
				readPDU(ctx.T, conn)

				ctx.Expect(awaitBound(smsc, "silent", true)).To(specs.BeTrue())
			})
		})

		s.When("a sink is registered before the client binds", func(s *specs.Spec) {
			s.It("reports the bind under the system_id that bound", func(ctx *specs.Context) {
				smsc := NewSmsc(false)
				collector := &eventCollector{}
				smsc.SetEventSink(collector.record)
				conn := startSmsc(ctx.T, smsc)

				writePDU(ctx.T, conn, stringBodyPDU(BIND_TRANSCEIVER, STS_OK, 1, "observed"))
				readPDU(ctx.T, conn)

				event, found := collector.await(EventBind, 0)
				ctx.Expect(found).To(specs.BeTrue())
				ctx.Expect(event.SystemId).ToEqual("observed")
				ctx.Expect(event.At.IsZero()).To(specs.BeFalse())
			})

			s.It("reports every inbound PDU with its command_id and sequence_number", func(ctx *specs.Context) {
				smsc := NewSmsc(false)
				collector := &eventCollector{}
				smsc.SetEventSink(collector.record)
				conn := startSmsc(ctx.T, smsc)

				writePDU(ctx.T, conn, stringBodyPDU(BIND_TRANSCEIVER, STS_OK, 1, "observed"))
				readPDU(ctx.T, conn)
				writePDU(ctx.T, conn, headerPDU(ENQUIRE_LINK, STS_OK, 7))
				readPDU(ctx.T, conn)

				event, found := collector.await(EventInboundPdu, ENQUIRE_LINK)
				ctx.Expect(found).To(specs.BeTrue())
				ctx.Expect(event.SequenceNum).ToEqual(uint32(7))
				ctx.Expect(event.SystemId).ToEqual("observed")
			})

			s.It("reports every outbound PDU with the command_id that went on the wire", func(ctx *specs.Context) {
				smsc := NewSmsc(false)
				collector := &eventCollector{}
				smsc.SetEventSink(collector.record)
				conn := startSmsc(ctx.T, smsc)

				writePDU(ctx.T, conn, stringBodyPDU(BIND_TRANSCEIVER, STS_OK, 3, "observed"))
				readPDU(ctx.T, conn)

				event, found := collector.await(EventOutboundPdu, BIND_TRANSCEIVER_RESP)
				ctx.Expect(found).To(specs.BeTrue())
				ctx.Expect(event.SequenceNum).ToEqual(uint32(3))
			})

			s.It("reports the unbind that removes the session", func(ctx *specs.Context) {
				smsc := NewSmsc(false)
				collector := &eventCollector{}
				smsc.SetEventSink(collector.record)
				conn := startSmsc(ctx.T, smsc)

				writePDU(ctx.T, conn, stringBodyPDU(BIND_TRANSCEIVER, STS_OK, 1, "leaving"))
				readPDU(ctx.T, conn)
				writePDU(ctx.T, conn, headerPDU(UNBIND, STS_OK, 2))
				readPDU(ctx.T, conn)

				event, found := collector.await(EventUnbind, 0)
				ctx.Expect(found).To(specs.BeTrue())
				ctx.Expect(event.SystemId).ToEqual("leaving")
			})
		})

		s.When("a subscriber registers while events are being emitted", func(s *specs.Spec) {
			s.It("keeps the sink free of data races", func(ctx *specs.Context) {
				smsc := NewSmsc(false)
				collector := &eventCollector{}

				var wg sync.WaitGroup
				wg.Add(32)
				for i := 0; i < 16; i++ {
					go func() {
						defer wg.Done()
						smsc.SetEventSink(collector.record)
					}()
					go func() {
						defer wg.Done()
						smsc.emitEvent(Event{Kind: EventInboundPdu, SystemId: "churn", CommandId: ENQUIRE_LINK})
					}()
				}
				wg.Wait()

				smsc.SetEventSink(nil)
				smsc.emitEvent(Event{Kind: EventInboundPdu, SystemId: "churn"})

				ctx.Expect(len(collector.snapshot()) >= 0).To(specs.BeTrue())
			})
		})
	})
}

func TestSmscSessionSnapshot(t *testing.T) {
	specs.Describe(t, "the session snapshot a TUI renders", func(s *specs.Spec) {
		s.When("several sessions are bound", func(s *specs.Spec) {
			s.It("returns one entry per live session", func(ctx *specs.Context) {
				smsc := NewSmsc(false)
				client, server := net.Pipe()
				defer client.Close()
				defer server.Close()

				smsc.addSession(2, Session{SystemId: "beta", Conn: server, ReceiveMo: true})
				smsc.addSession(1, Session{SystemId: "alpha", Conn: server, ReceiveMo: false})

				ctx.Expect(len(smsc.SessionSnapshot())).ToEqual(2)
			})

			s.It("orders the entries deterministically so the view does not jitter", func(ctx *specs.Context) {
				client, server := net.Pipe()
				defer client.Close()
				defer server.Close()

				for attempt := 0; attempt < 50; attempt++ {
					smsc := NewSmsc(false)
					smsc.addSession(2, Session{SystemId: "beta", Conn: server})
					smsc.addSession(1, Session{SystemId: "alpha", Conn: server})
					smsc.addSession(3, Session{SystemId: "gamma", Conn: server})

					snapshot := smsc.SessionSnapshot()
					ctx.Expect(len(snapshot)).ToEqual(3)
					ctx.Expect(snapshot[0].SystemId).ToEqual("alpha")
					ctx.Expect(snapshot[1].SystemId).ToEqual("beta")
					ctx.Expect(snapshot[2].SystemId).ToEqual("gamma")
				}
			})

			s.It("carries the bind type, the bind time and the remote address", func(ctx *specs.Context) {
				smsc := NewSmsc(false)
				client, server := net.Pipe()
				defer client.Close()
				defer server.Close()

				boundAt := time.Now()
				smsc.addSession(1, Session{
					SystemId:   "alpha",
					Conn:       server,
					ReceiveMo:  true,
					BindType:   BindTransceiver,
					BoundAt:    boundAt,
					RemoteAddr: "127.0.0.1:5000",
				})

				info := smsc.SessionSnapshot()[0]
				ctx.Expect(info.SessionId).ToEqual(1)
				ctx.Expect(info.BindType).ToEqual(BindTransceiver)
				ctx.Expect(info.RemoteAddr).ToEqual("127.0.0.1:5000")
				ctx.Expect(info.BoundAt.Equal(boundAt)).To(specs.BeTrue())
				ctx.Expect(info.ReceiveMo).To(specs.BeTrue())
			})

			s.It("never exposes the network connection to the caller", func(ctx *specs.Context) {
				infoType := reflect.TypeOf(SessionInfo{})
				connInterface := reflect.TypeOf((*net.Conn)(nil)).Elem()

				leaks := false
				for i := 0; i < infoType.NumField(); i++ {
					if infoType.Field(i).Type.Implements(connInterface) {
						leaks = true
					}
				}

				ctx.Expect(leaks).To(specs.BeFalse())
			})
		})

		s.When("a real client binds as a receiver", func(s *specs.Spec) {
			s.It("records the bind type the client actually used", func(ctx *specs.Context) {
				smsc := NewSmsc(false)
				conn := startSmsc(ctx.T, smsc)

				writePDU(ctx.T, conn, stringBodyPDU(BIND_RECEIVER, STS_OK, 1, "listener"))
				readPDU(ctx.T, conn)
				ctx.Expect(awaitBound(smsc, "listener", true)).To(specs.BeTrue())

				info := smsc.SessionSnapshot()[0]
				ctx.Expect(info.BindType).ToEqual(BindReceiver)
				ctx.Expect(info.ReceiveMo).To(specs.BeTrue())
				ctx.Expect(info.RemoteAddr == "").To(specs.BeFalse())
				ctx.Expect(info.BoundAt.IsZero()).To(specs.BeFalse())
			})
		})
	})
}
