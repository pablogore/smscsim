package main

import (
	"log"
	"sync/atomic"
)

// logEvent writes one event the way the simulator has always written it. The log is no longer where
// the simulator records what it did; it is one view of the event stream, registered like any other.
//
// Text already carries exactly what the corresponding log.Printf produced, including its trailing
// newline where it had one, and log.Print adds a newline only when the line lacks it. That is what
// keeps the simulator's output byte for byte what it always was.
func logEvent(e Event) {
	if e.Text == "" {
		return
	}
	log.Print(e.Text)
}

// LogDropNotice is the sentence the log uses to admit it is incomplete.
const LogDropNotice = "log incomplete: %d event(s) were dropped before this line"

// logSubscriber is the simulator's default subscriber: logEvent plus the one thing a bounded queue
// owes its reader.
//
// The logging subscriber is bounded like every other subscriber, because exempting it would put the
// log back at the centre of the design and would take away the memory bound that makes publishing
// safe from every connection goroutine. What it must not do is lose events silently. So before it
// writes a line it compares the publisher's drop counter against what it has already admitted to,
// and if the counter has grown it says so first. A reader of the log then knows exactly where the
// log stopped being complete and by how much.
//
// This is deliberately driven by the counter and not by a ticker or a deadline: the notice appears
// at a point in the output that follows from the events alone, so the same event sequence always
// produces the same bytes.
type logSubscriber struct {
	// source is the subscription this subscriber was registered under. It is set once, just after
	// Subscribe returns, and read from the delivery goroutine, so it is atomic. Until it is set the
	// count reads as zero; nothing is lost by that, because a drop that happens in that window is
	// still in the counter and is announced before the next line.
	source atomic.Pointer[Subscription]
	// reported is how many drops have already been admitted. Only the delivery goroutine touches
	// it, and a subscriber's callback runs in one goroutine at a time, so it needs no lock.
	reported uint64
}

// bind tells the subscriber which subscription counts its drops.
func (l *logSubscriber) bind(sub *Subscription) {
	l.source.Store(sub)
}

// handle is the subscriber function itself: announce any loss since the last line, then write the
// line.
func (l *logSubscriber) handle(e Event) {
	if dropped := l.source.Load().Dropped(); dropped > l.reported {
		log.Printf(LogDropNotice, dropped-l.reported)
		l.reported = dropped
	}
	logEvent(e)
}
