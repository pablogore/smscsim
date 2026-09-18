package main

import (
	"encoding/binary"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getsyntegrity/go-specs/specs"
)

func TestEventPublisher(t *testing.T) {
	specs.Describe(t, "The event publisher", func(s *specs.Spec) {
		s.When("an event is published and someone is listening", func(s *specs.Spec) {
			s.It("hands the subscriber exactly the event that was published", func(ctx *specs.Context) {
				publisher := NewPublisher()
				received := make(chan Event, 1)
				sub := publisher.Subscribe(func(e Event) { received <- e })
				defer sub.Unsubscribe()

				published := Event{
					At:        time.Unix(1700000000, 0).UTC(),
					Kind:      KindBindRequest,
					Direction: Inbound,
					SystemId:  "smppclient",
					SessionId: 42,
					Text:      "bind request from system_id[smppclient]\n",
				}
				publisher.Publish(published)

				got, ok := awaitAnyEvent(received)
				ctx.Expect(ok).To(specs.BeTrue())
				ctx.Expect(got).ToEqual(published)
			})
		})

		s.When("several subscribers are listening at once", func(s *specs.Spec) {
			s.It("hands every one of them the same event", func(ctx *specs.Context) {
				publisher := NewPublisher()

				const listeners = 3
				inboxes := make([]chan Event, listeners)
				for i := range inboxes {
					inbox := make(chan Event, 1)
					inboxes[i] = inbox
					sub := publisher.Subscribe(func(e Event) { inbox <- e })
					defer sub.Unsubscribe()
				}

				published := Event{Kind: KindEnquireLink, Direction: Inbound, SystemId: "smppclient", SessionId: 7}
				publisher.Publish(published)

				for _, inbox := range inboxes {
					got, ok := awaitAnyEvent(inbox)
					ctx.Expect(ok).To(specs.BeTrue())
					ctx.Expect(got).ToEqual(published)
				}
			})
		})

		s.When("no one is listening", func(s *specs.Spec) {
			s.It("accepts the event and keeps the publisher usable", func(ctx *specs.Context) {
				publisher := NewPublisher()
				publisher.Publish(Event{Kind: KindUnbindRequest, Direction: Inbound, SystemId: "nobody"})

				received := make(chan Event, 1)
				sub := publisher.Subscribe(func(e Event) { received <- e })
				defer sub.Unsubscribe()

				expected := Event{Kind: KindUnbindRequest, Direction: Inbound, SystemId: "somebody"}
				publisher.Publish(expected)

				got, ok := awaitAnyEvent(received)
				ctx.Expect(ok).To(specs.BeTrue())
				ctx.Expect(got).ToEqual(expected)
			})
		})

		s.When("many connections publish at the same time", func(s *specs.Spec) {
			s.It("delivers every event it accepted and drops none it had room for", func(ctx *specs.Context) {
				const publishers = 24
				const perPublisher = 40
				const total = publishers * perPublisher

				publisher := NewPublisher()
				counted := make(chan struct{}, total)
				sub := publisher.SubscribeBuffered(total, func(e Event) { counted <- struct{}{} })
				defer sub.Unsubscribe()

				var wg sync.WaitGroup
				for p := 0; p < publishers; p++ {
					wg.Add(1)
					go func(id int) {
						defer wg.Done()
						for i := 0; i < perPublisher; i++ {
							publisher.Publish(Event{Kind: KindSubmitSm, Direction: Inbound, SessionId: id})
						}
					}(p)
				}
				wg.Wait()

				deadline := time.After(5 * time.Second)
				delivered := 0
				for delivered < total {
					select {
					case <-counted:
						delivered++
					case <-deadline:
						specs.ExpectT(ctx, delivered).ToEqual(total)
						return
					}
				}

				specs.ExpectT(ctx, delivered).ToEqual(total)
				specs.ExpectT(ctx, sub.Dropped()).ToEqual(uint64(0))
			})
		})

		s.When("a subscriber stops reading its events", func(s *specs.Spec) {
			s.It("keeps accepting events instead of blocking the caller", func(ctx *specs.Context) {
				publisher, sub, release := stalledSubscriber(4)
				defer close(release)
				defer sub.Unsubscribe()

				published := make(chan struct{})
				go func() {
					defer close(published)
					for i := 0; i < 40; i++ {
						publisher.Publish(Event{Kind: KindSubmitSm, Direction: Inbound})
					}
				}()

				finished := false
				select {
				case <-published:
					finished = true
				case <-time.After(5 * time.Second):
				}
				ctx.Expect(finished).To(specs.BeTrue())

				// the publisher only proves it sheds load if it really was under backpressure
				ctx.Expect(sub.Dropped() > 0).To(specs.BeTrue())
			})

			s.It("counts the events it had to drop", func(ctx *specs.Context) {
				const buffer = 4
				const overflow = 7

				publisher, sub, release := stalledSubscriber(buffer)
				defer close(release)
				defer sub.Unsubscribe()

				for i := 0; i < buffer; i++ {
					publisher.Publish(Event{Kind: KindSubmitSm, Direction: Inbound})
				}
				specs.ExpectT(ctx, sub.Dropped()).ToEqual(uint64(0))

				for i := 0; i < overflow; i++ {
					publisher.Publish(Event{Kind: KindSubmitSm, Direction: Inbound})
				}
				specs.ExpectT(ctx, sub.Dropped()).ToEqual(uint64(overflow))
			})
		})

		s.When("a subscriber unsubscribes", func(s *specs.Spec) {
			s.It("stops handing it events published afterwards", func(ctx *specs.Context) {
				publisher := NewPublisher()
				received := make(chan Event, 8)
				sub := publisher.Subscribe(func(e Event) { received <- e })

				publisher.Publish(Event{Kind: KindBindRequest, Direction: Inbound, SystemId: "before"})
				got, ok := awaitAnyEvent(received)
				ctx.Expect(ok).To(specs.BeTrue())
				specs.ExpectT(ctx, got.SystemId).ToEqual("before")

				sub.Unsubscribe()
				publisher.Publish(Event{Kind: KindBindRequest, Direction: Inbound, SystemId: "after"})

				_, arrived := awaitAnyEvent(received)
				ctx.Expect(arrived).To(specs.BeFalse())
			})
		})
	})
}

func TestEventLogging(t *testing.T) {
	specs.Describe(t, "The logging subscriber", func(s *specs.Spec) {
		s.When("an event reaches the log", func(s *specs.Spec) {
			s.It("writes the same line the simulator has always written", func(ctx *specs.Context) {
				written := &syncBuffer{}
				previous := log.Writer()
				previousFlags := log.Flags()
				log.SetOutput(written)
				log.SetFlags(0)
				defer func() {
					log.SetOutput(previous)
					log.SetFlags(previousFlags)
				}()

				logEvent(Event{
					Kind:      KindBindRequest,
					Direction: Inbound,
					SystemId:  "smppclient",
					Text:      "bind request from system_id[smppclient]\n",
				})

				specs.ExpectT(ctx, written.String()).ToEqual("bind request from system_id[smppclient]\n")
			})
		})
	})
}

func TestSimulatorPublishesPduEvents(t *testing.T) {
	specs.Describe(t, "The simulator at the PDU boundary", func(s *specs.Spec) {
		s.When("a client binds", func(s *specs.Spec) {
			s.It("announces the bind as an inbound event naming the system id", func(ctx *specs.Context) {
				events, client, closeSession := connectedSession()
				defer closeSession()

				client.Write(stringBodyPDU(BIND_TRANSCEIVER, STS_OK, 1, "smppclient"))
				drainPDU(client)

				bind, ok := awaitEvent(events, func(e Event) bool { return e.Kind == KindBindRequest })
				ctx.Expect(ok).To(specs.BeTrue())
				specs.ExpectT(ctx, bind.SystemId).ToEqual("smppclient")
				specs.ExpectT(ctx, bind.Direction).ToEqual(Inbound)
				specs.ExpectT(ctx, strings.Contains(bind.Text, "smppclient")).ToEqual(true)
				ctx.Expect(bind.At.IsZero()).To(specs.BeFalse())
			})
		})

		s.When("a bound client unbinds", func(s *specs.Spec) {
			s.It("announces the unbind under the same session as the bind", func(ctx *specs.Context) {
				events, client, closeSession := connectedSession()
				defer closeSession()

				client.Write(stringBodyPDU(BIND_TRANSCEIVER, STS_OK, 1, "smppclient"))
				drainPDU(client)
				bind, bound := awaitEvent(events, func(e Event) bool { return e.Kind == KindBindRequest })
				ctx.Expect(bound).To(specs.BeTrue())

				client.Write(headerPDU(UNBIND, STS_OK, 2))
				drainPDU(client)

				unbind, ok := awaitEvent(events, func(e Event) bool { return e.Kind == KindUnbindRequest })
				ctx.Expect(ok).To(specs.BeTrue())
				specs.ExpectT(ctx, unbind.SessionId).ToEqual(bind.SessionId)
				specs.ExpectT(ctx, unbind.Direction).ToEqual(Inbound)
			})
		})
	})
}

// helpers

// awaitAnyEvent waits a bounded time for one event, so a spec that expects silence fails fast
// instead of hanging the suite.
func awaitAnyEvent(inbox <-chan Event) (Event, bool) {
	select {
	case e := <-inbox:
		return e, true
	case <-time.After(500 * time.Millisecond):
		return Event{}, false
	}
}

func awaitEvent(inbox <-chan Event, match func(Event) bool) (Event, bool) {
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-inbox:
			if match(e) {
				return e, true
			}
		case <-deadline:
			return Event{}, false
		}
	}
}

// stalledSubscriber returns a publisher whose only subscriber is parked inside its callback, so
// its buffer is empty and its whole capacity is known to the caller. Closing the returned channel
// lets it go.
func stalledSubscriber(buffer int) (*Publisher, *Subscription, chan struct{}) {
	publisher := NewPublisher()
	entered := make(chan struct{})
	release := make(chan struct{})

	var once sync.Once
	sub := publisher.SubscribeBuffered(buffer, func(e Event) {
		once.Do(func() {
			close(entered)
			<-release
		})
	})

	publisher.Publish(Event{Kind: KindSubmitSm, Direction: Inbound})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		// the subscriber never ran, so the spec that follows reports it instead of hanging
	}

	return publisher, sub, release
}

// connectedSession drives the real PDU read loop over an in-memory connection, so the specs
// observe events the simulator publishes and not a hand-rolled imitation.
func connectedSession() (<-chan Event, net.Conn, func()) {
	smsc := NewSmsc(false)
	events := make(chan Event, 64)
	sub := smsc.Subscribe(func(e Event) { events <- e })

	client, server := net.Pipe()
	go handleSmppConnection(smsc, server)

	return events, client, func() {
		sub.Unsubscribe()
		client.Close()
	}
}

// drainPDU reads one whole response, so the unbuffered pipe cannot stall the read loop.
func drainPDU(conn net.Conn) {
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	head := make([]byte, 16)
	if _, err := io.ReadFull(conn, head); err != nil {
		return
	}
	cmdLen := binary.BigEndian.Uint32(head[0:])
	if cmdLen <= 16 {
		return
	}
	io.ReadFull(conn, make([]byte, cmdLen-16))
}
