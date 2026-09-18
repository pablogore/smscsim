package main

import (
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/getsyntegrity/go-specs/specs"
)

func TestSessionStore(t *testing.T) {
	specs.Describe(t, "The session store", func(s *specs.Spec) {
		s.When("many connections bind, read and unbind at the same time", func(s *specs.Spec) {
			// regression for the "concurrent map writes" crash: two smpp clients binding at the same
			// time used to write the session map with no lock at all
			s.It("serves every one of them without a data race", func(ctx *specs.Context) {
				smsc := NewSmsc(false)

				client, server := net.Pipe()
				defer client.Close()
				defer server.Close()

				const goroutines = 16
				var wg sync.WaitGroup
				wg.Add(goroutines * 3)

				for i := 0; i < goroutines; i++ {
					sessionId := i
					go func() {
						defer wg.Done()
						smsc.addSession(sessionId, Session{SystemId: "system_id", Conn: server, ReceiveMo: true})
					}()
					go func() {
						defer wg.Done()
						smsc.BoundSystemIds()
					}()
					go func() {
						defer wg.Done()
						smsc.removeSession(sessionId)
					}()
				}

				wg.Wait()
			})
		})

		s.When("one MO-capable session is bound", func(s *specs.Spec) {
			s.It("finds it by its system id and reports that it can carry MO messages", func(ctx *specs.Context) {
				smsc := NewSmsc(false)
				client, server := net.Pipe()
				defer client.Close()
				defer server.Close()

				smsc.addSession(1, Session{SystemId: "transceiver", Conn: server, ReceiveMo: true})

				sess, found := smsc.findSession("transceiver")
				ctx.Expect(found).To(specs.BeTrue())
				ctx.Expect(sess.ReceiveMo).To(specs.BeTrue())
			})
		})

		s.When("nothing is bound under a system id", func(s *specs.Spec) {
			s.It("reports that no session was found", func(ctx *specs.Context) {
				smsc := NewSmsc(false)
				client, server := net.Pipe()
				defer client.Close()
				defer server.Close()

				smsc.addSession(1, Session{SystemId: "transceiver", Conn: server, ReceiveMo: true})

				_, found := smsc.findSession("unknown")
				ctx.Expect(found).To(specs.BeFalse())
			})
		})

		s.When("a transmitter and a receiver share one system id", func(s *specs.Spec) {
			// map iteration order is randomized, so this runs often enough to make a first-match
			// implementation lose
			s.It("always returns the receiver, whatever order the map offers them in", func(ctx *specs.Context) {
				client, server := net.Pipe()
				defer client.Close()
				defer server.Close()

				for i := 0; i < 200; i++ {
					smsc := NewSmsc(false)
					smsc.addSession(1, Session{SystemId: "smppclient", Conn: server, ReceiveMo: false})
					smsc.addSession(2, Session{SystemId: "smppclient", Conn: server, ReceiveMo: true})

					sess, found := smsc.findSession("smppclient")
					ctx.Expect(found).To(specs.BeTrue())
					ctx.Expect(sess.ReceiveMo).To(specs.BeTrue())
				}
			})
		})

		s.When("the only bound session cannot carry MO messages", func(s *specs.Spec) {
			s.It("still returns it, so the caller can refuse it explicitly", func(ctx *specs.Context) {
				client, server := net.Pipe()
				defer client.Close()
				defer server.Close()

				smsc := NewSmsc(false)
				smsc.addSession(1, Session{SystemId: "smppclient", Conn: server, ReceiveMo: false})

				sess, found := smsc.findSession("smppclient")
				ctx.Expect(found).To(specs.BeTrue())
				ctx.Expect(sess.ReceiveMo).To(specs.BeFalse())
			})
		})

		s.When("a session is filed under a key", func(s *specs.Spec) {
			s.It("hands the session back knowing the key it was filed under", func(ctx *specs.Context) {
				smsc := NewSmsc(false)
				client, server := net.Pipe()
				defer client.Close()
				defer server.Close()

				smsc.addSession(4711, Session{SystemId: "smppclient", Conn: server, ReceiveMo: true})

				sess, found := smsc.findSession("smppclient")
				ctx.Expect(found).To(specs.BeTrue())
				specs.ExpectT(ctx, sess.Id).ToEqual(4711)
			})

			s.It("overrules any id the caller put on the session, so the two cannot disagree", func(ctx *specs.Context) {
				smsc := NewSmsc(false)
				client, server := net.Pipe()
				defer client.Close()
				defer server.Close()

				smsc.addSession(4711, Session{Id: 9, SystemId: "smppclient", Conn: server, ReceiveMo: true})

				sess, found := smsc.findSession("smppclient")
				ctx.Expect(found).To(specs.BeTrue())
				specs.ExpectT(ctx, sess.Id).ToEqual(4711)
			})
		})
	})
}

func TestSendingAnMoMessage(t *testing.T) {
	specs.Describe(t, "Sending an MO message", func(s *specs.Spec) {
		s.When("the deliver_sm reaches the client", func(s *specs.Spec) {
			s.It("announces it as outbound, naming the session it went to", func(ctx *specs.Context) {
				smsc, events := listeningSimulator()
				server := drainedSession(smsc, 4711, "smppclient", true)
				defer server.Close()

				err := smsc.SendMoMessage("sender", "recipient", "hello", "smppclient")
				ctx.Expect(err == nil).To(specs.BeTrue())

				sent, ok := awaitEvent(events, func(e Event) bool { return e.Kind == KindMoMessage })
				ctx.Expect(ok).To(specs.BeTrue())
				specs.ExpectT(ctx, sent.Direction).ToEqual(Outbound)
				specs.ExpectT(ctx, sent.SystemId).ToEqual("smppclient")
				specs.ExpectT(ctx, sent.SessionId).ToEqual(4711)
				specs.ExpectT(ctx, sent.Text).ToEqual(
					"MO message to systemId: [smppclient] was successfully sent. Sender: [sender], recipient: [recipient]")
			})
		})

		s.When("no session is bound for the system id", func(s *specs.Spec) {
			s.It("refuses the request and announces the refusal as inbound", func(ctx *specs.Context) {
				smsc, events := listeningSimulator()

				err := smsc.SendMoMessage("sender", "recipient", "hello", "nobody")
				ctx.Expect(err != nil).To(specs.BeTrue())

				refused, ok := awaitEvent(events, func(e Event) bool { return e.Kind == KindMoNoSession })
				ctx.Expect(ok).To(specs.BeTrue())
				specs.ExpectT(ctx, refused.Direction).ToEqual(Inbound)
				specs.ExpectT(ctx, refused.SystemId).ToEqual("nobody")
				specs.ExpectT(ctx, refused.SessionId).ToEqual(0)
				specs.ExpectT(ctx, refused.Text).ToEqual(
					"Cannot send MO message to systemId: [nobody]. No bound session found")
			})
		})

		s.When("the bound session cannot receive MO messages", func(s *specs.Spec) {
			s.It("refuses the request and names the session that could not take it", func(ctx *specs.Context) {
				smsc, events := listeningSimulator()
				server := drainedSession(smsc, 1234, "transmitter", false)
				defer server.Close()

				err := smsc.SendMoMessage("sender", "recipient", "hello", "transmitter")
				ctx.Expect(err != nil).To(specs.BeTrue())

				refused, ok := awaitEvent(events, func(e Event) bool { return e.Kind == KindMoBindCannotReceive })
				ctx.Expect(ok).To(specs.BeTrue())
				specs.ExpectT(ctx, refused.Direction).ToEqual(Inbound)
				specs.ExpectT(ctx, refused.SystemId).ToEqual("transmitter")
				specs.ExpectT(ctx, refused.SessionId).ToEqual(1234)
				specs.ExpectT(ctx, refused.Text).ToEqual(
					"Cannot send MO message to systemId: [transmitter]. Only RECEIVER and TRANSCEIVER sessions could receive MO messages")
			})
		})

		s.When("writing the deliver_sm to the connection fails", func(s *specs.Spec) {
			s.It("announces the failure as outbound, naming the session and the network error", func(ctx *specs.Context) {
				smsc, events := listeningSimulator()

				client, server := net.Pipe()
				client.Close()
				server.Close()
				smsc.addSession(99, Session{SystemId: "smppclient", Conn: server, ReceiveMo: true})

				err := smsc.SendMoMessage("sender", "recipient", "hello", "smppclient")
				ctx.Expect(err != nil).To(specs.BeTrue())

				failed, ok := awaitEvent(events, func(e Event) bool { return e.Kind == KindMoSendFailed })
				ctx.Expect(ok).To(specs.BeTrue())
				specs.ExpectT(ctx, failed.Direction).ToEqual(Outbound)
				specs.ExpectT(ctx, failed.SystemId).ToEqual("smppclient")
				specs.ExpectT(ctx, failed.SessionId).ToEqual(99)
				ctx.Expect(strings.HasPrefix(failed.Text,
					"Cannot send MO message to systemId: [smppclient]. Network error [")).To(specs.BeTrue())
				ctx.Expect(strings.HasSuffix(failed.Text, "]")).To(specs.BeTrue())
			})
		})
	})
}

// helpers

// listeningSimulator is a simulator with a spec-owned subscriber attached, so a spec reads the
// events the simulator really published rather than a hand-rolled imitation.
func listeningSimulator() (*Smsc, <-chan Event) {
	smsc := NewSmsc(false)
	events := make(chan Event, 64)
	smsc.Subscribe(func(e Event) { events <- e })
	return smsc, events
}

// drainedSession files a session on an in-memory connection whose far end is being read, so a
// deliver_sm written to it completes instead of parking on the unbuffered pipe.
func drainedSession(smsc *Smsc, sessionId int, systemId string, receiveMo bool) net.Conn {
	client, server := net.Pipe()
	go io.Copy(io.Discard, client)
	smsc.addSession(sessionId, Session{SystemId: systemId, Conn: server, ReceiveMo: receiveMo})
	return server
}
