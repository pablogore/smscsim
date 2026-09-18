package main

import (
	"log"
	"testing"

	"github.com/getsyntegrity/go-specs/specs"
)

func TestLogSubscriberAnnouncesItsOwnLosses(t *testing.T) {
	specs.Describe(t, "The logging subscriber", func(s *specs.Spec) {
		s.When("the publisher has dropped nothing", func(s *specs.Spec) {
			s.It("writes exactly the line the event carries, and nothing else", func(ctx *specs.Context) {
				subscriber, _ := boundLogSubscriber()

				written := withCapturedLog(func() {
					subscriber.handle(Event{
						Kind:      KindBindRequest,
						Direction: Inbound,
						SystemId:  "smppclient",
						Text:      "bind request from system_id[smppclient]\n",
					})
				})

				specs.ExpectT(ctx, written).ToEqual("bind request from system_id[smppclient]\n")
			})
		})

		s.When("the publisher dropped events before the next line", func(s *specs.Spec) {
			s.It("says how many events were lost, immediately before that line", func(ctx *specs.Context) {
				subscriber, dropping := boundLogSubscriber()
				dropping.dropped.Add(3)

				written := withCapturedLog(func() {
					subscriber.handle(Event{Kind: KindSubmitSm, Text: "submit_sm from system_id[smppclient]\n"})
				})

				specs.ExpectT(ctx, written).ToEqual(
					"log incomplete: 3 event(s) were dropped before this line\n" +
						"submit_sm from system_id[smppclient]\n")
			})

			s.It("does not repeat the announcement on the next line", func(ctx *specs.Context) {
				subscriber, dropping := boundLogSubscriber()
				dropping.dropped.Add(3)

				first := withCapturedLog(func() {
					subscriber.handle(Event{Kind: KindSubmitSm, Text: "first line\n"})
				})
				second := withCapturedLog(func() {
					subscriber.handle(Event{Kind: KindSubmitSm, Text: "second line\n"})
				})

				// asserting the announcement really happened first is what stops this from being a
				// spec that a subscriber which never announces anything would also satisfy
				specs.ExpectT(ctx, first).ToEqual(
					"log incomplete: 3 event(s) were dropped before this line\n" +
						"first line\n")
				specs.ExpectT(ctx, second).ToEqual("second line\n")
			})

			s.It("counts only the events lost since the last announcement", func(ctx *specs.Context) {
				subscriber, dropping := boundLogSubscriber()
				dropping.dropped.Add(3)

				withCapturedLog(func() {
					subscriber.handle(Event{Kind: KindSubmitSm, Text: "first line\n"})
				})
				dropping.dropped.Add(2)
				written := withCapturedLog(func() {
					subscriber.handle(Event{Kind: KindSubmitSm, Text: "second line\n"})
				})

				specs.ExpectT(ctx, written).ToEqual(
					"log incomplete: 2 event(s) were dropped before this line\n" +
						"second line\n")
			})

			s.It("still announces the loss when the event it carries has nothing to write", func(ctx *specs.Context) {
				subscriber, dropping := boundLogSubscriber()
				dropping.dropped.Add(1)

				written := withCapturedLog(func() {
					subscriber.handle(Event{Kind: KindSubmitSm})
				})

				specs.ExpectT(ctx, written).ToEqual("log incomplete: 1 event(s) were dropped before this line\n")
			})
		})

		s.When("it is not bound to a subscription yet", func(s *specs.Spec) {
			s.It("writes the line without announcing anything", func(ctx *specs.Context) {
				subscriber := &logSubscriber{}

				written := withCapturedLog(func() {
					subscriber.handle(Event{Kind: KindEnquireLink, Text: "enquire_link from system_id[smppclient]\n"})
				})

				specs.ExpectT(ctx, written).ToEqual("enquire_link from system_id[smppclient]\n")
			})
		})
	})
}

func TestSimulatorKeepsItsLoggingSubscription(t *testing.T) {
	specs.Describe(t, "The simulator's logging subscription", func(s *specs.Spec) {
		s.When("the simulator is created", func(s *specs.Spec) {
			s.It("keeps the handle so the log's own losses can be counted", func(ctx *specs.Context) {
				smsc := NewSmsc(false)

				logs := smsc.LogSubscription()
				ctx.Expect(logs != nil).To(specs.BeTrue())
				specs.ExpectT(ctx, logs.Dropped()).ToEqual(uint64(0))
			})
		})
	})
}

// helpers

// boundLogSubscriber returns a logging subscriber bound to a subscription the spec can drop events
// on directly. Nothing here depends on a goroutine, a buffer filling up or a clock, so the
// announcement is exercised exactly, run after run.
func boundLogSubscriber() (*logSubscriber, *Subscription) {
	dropping := &Subscription{}
	subscriber := &logSubscriber{}
	subscriber.bind(dropping)
	return subscriber, dropping
}

// withCapturedLog collects everything fn writes to the standard logger, with the flags off so the
// captured bytes are the line itself and nothing the logger added.
func withCapturedLog(fn func()) string {
	written := &syncBuffer{}
	previousOutput := log.Writer()
	previousFlags := log.Flags()
	log.SetOutput(written)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(previousOutput)
		log.SetFlags(previousFlags)
	}()

	fn()

	return written.String()
}
