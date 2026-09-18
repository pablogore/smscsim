package main

import (
	"sync"
	"sync/atomic"
	"time"
)

// The simulator used to describe everything it did with log.Printf, so the only way to observe a
// bind, a submit_sm or a delivery receipt was to read the process log back and parse it. This file
// gives those observations a shape: a typed Event published at the SMPP PDU boundary, and a
// publisher any number of subscribers can listen to. Writing the log is one of those subscribers.
//
// Nothing here knows about a screen, a renderer or a terminal. A subscriber is a plain func(Event),
// so a consumer is free to keep its own channel, its own goroutine and its own presentation.

// Direction says which way across the SMPP connection the event travelled.
type Direction uint8

const (
	// Inbound is something the client sent to the simulator, or a failure to read it.
	Inbound Direction = iota + 1
	// Outbound is something the simulator sent to the client, or a failure to send it.
	Outbound
)

// EventKind names what happened. The set is closed by what the PDU read loop actually reports:
// there is no kind here that nothing publishes.
type EventKind string

const (
	KindBindRequest           EventKind = "bind_request"
	KindBindRejected          EventKind = "bind_rejected"
	KindUnbindRequest         EventKind = "unbind_request"
	KindEnquireLink           EventKind = "enquire_link"
	KindSubmitSm              EventKind = "submit_sm"
	KindSubmitRejected        EventKind = "submit_rejected"
	KindDeliverSmResp         EventKind = "deliver_sm_resp"
	KindDeliveryReceipt       EventKind = "delivery_receipt"
	KindDeliveryReceiptFailed EventKind = "delivery_receipt_failed"
	KindUnsupportedPdu        EventKind = "unsupported_pdu"
	KindProtocolError         EventKind = "protocol_error"
	KindConnectionClosed      EventKind = "connection_closed"
)

// Event is one thing the simulator observed at the PDU boundary. Every field is a small value, so
// an Event is cheap to copy and safe to hand to any number of subscribers at once.
//
// Text is the sentence the simulator has always written to its log. It is kept on the event so the
// log can be an ordinary subscriber rather than the place the information lives; a consumer that
// wants structure reads Kind, Direction, SystemId and SessionId instead of parsing it.
type Event struct {
	At        time.Time
	Kind      EventKind
	Direction Direction
	SystemId  string
	SessionId int
	Text      string
}

// DefaultSubscriberBuffer is how many events a subscriber may fall behind by before the publisher
// starts dropping its events.
const DefaultSubscriberBuffer = 1024

// Publisher fans events out to its subscribers.
//
// Drop policy: publishing never blocks and never fails. Every subscriber owns a bounded queue and
// its own delivery goroutine. When that queue is full the event is dropped for that subscriber
// alone and counted on Subscription.Dropped. The SMPP read loop publishes from every connection
// goroutine, so blocking there would stall the server on whichever consumer happens to be slow,
// and an unbounded queue would let a stalled consumer grow memory without limit. Dropping is the
// honest third option, and the counter keeps it visible instead of silent.
type Publisher struct {
	mu     sync.RWMutex // guards subscribers and lastId
	subs   map[uint64]*Subscription
	lastId uint64
}

func NewPublisher() *Publisher {
	return &Publisher{subs: make(map[uint64]*Subscription)}
}

// Subscribe registers fn with the default buffer.
func (p *Publisher) Subscribe(fn func(Event)) *Subscription {
	return p.SubscribeBuffered(DefaultSubscriberBuffer, fn)
}

// SubscribeBuffered registers fn with a queue of its own size. fn is called from a single
// goroutine, one event at a time, in publication order, so a subscriber needs no lock of its own.
func (p *Publisher) SubscribeBuffered(buffer int, fn func(Event)) *Subscription {
	if buffer < 1 {
		buffer = 1
	}
	if fn == nil {
		fn = func(Event) {}
	}

	sub := &Subscription{
		publisher: p,
		events:    make(chan Event, buffer),
		done:      make(chan struct{}),
	}

	p.mu.Lock()
	p.lastId++
	sub.id = p.lastId
	p.subs[sub.id] = sub
	p.mu.Unlock()

	go sub.deliver(fn)

	return sub
}

// Publish hands the event to every subscriber that has room for it. It never blocks and is safe to
// call with no subscribers at all.
func (p *Publisher) Publish(e Event) {
	if p == nil {
		return
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	for _, sub := range p.subs {
		select {
		case sub.events <- e:
		default:
			sub.dropped.Add(1)
		}
	}
}

// Subscription is a handle on one registered subscriber.
type Subscription struct {
	publisher *Publisher
	id        uint64
	events    chan Event
	done      chan struct{}
	stop      sync.Once
	dropped   atomic.Uint64
}

// Unsubscribe stops delivery. Once it returns, no event published afterwards reaches the
// subscriber. Events still queued are abandoned rather than flushed, because a subscriber that has
// gone away has no use for them.
func (s *Subscription) Unsubscribe() {
	if s == nil {
		return
	}

	s.stop.Do(func() {
		// leaving the registry first is what makes the guarantee above hold: after this, Publish
		// can no longer find the subscription to enqueue anything on it
		if s.publisher != nil {
			s.publisher.mu.Lock()
			delete(s.publisher.subs, s.id)
			s.publisher.mu.Unlock()
		}
		close(s.done)
	})
}

// Dropped reports how many events this subscriber was too slow to take.
func (s *Subscription) Dropped() uint64 {
	if s == nil {
		return 0
	}
	return s.dropped.Load()
}

func (s *Subscription) deliver(fn func(Event)) {
	for {
		select {
		case <-s.done:
			return
		case e := <-s.events:
			fn(e)
		}
	}
}
