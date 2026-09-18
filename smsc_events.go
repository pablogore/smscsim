package main

import (
	"encoding/binary"
	"net"
	"sort"
	"sync"
	"time"
)

// syncConn serializes writes to one accepted connection.
//
// Smsc.mu guards the session map, not the socket. Without this wrapper a multi-part MO message
// written by SendMoMessage can interleave with a response PDU written by the read loop or with
// the delivery receipt written by its own goroutine, and the client sees a corrupted SMPP
// stream. Only Write is guarded; reads, deadlines and Close keep the embedded behaviour, and
// because syncConn satisfies net.Conn every existing call site keeps working unchanged.
type syncConn struct {
	net.Conn
	writeMu sync.Mutex
}

func newSyncConn(conn net.Conn) *syncConn {
	return &syncConn{Conn: conn}
}

func (c *syncConn) Write(b []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.Conn.Write(b)
}

// EventKind names what happened on a connection.
type EventKind string

const (
	EventBind        EventKind = "bind"
	EventUnbind      EventKind = "unbind"
	EventInboundPdu  EventKind = "inbound_pdu"
	EventOutboundPdu EventKind = "outbound_pdu"
)

// Event is one observable thing the simulator did, shaped for a subscriber such as a TUI.
// CommandId and SequenceNum are zero for events that are not tied to a single PDU.
type Event struct {
	At          time.Time
	Kind        EventKind
	SystemId    string
	CommandId   uint32
	SequenceNum uint32
	Detail      string
}

// EventSink receives every Event the simulator emits.
//
// It is called SYNCHRONOUSLY on the connection goroutine that produced the event, so an
// implementation must not block: hand the event to a channel or a buffer and return. A slow
// sink slows down the SMPP connection it is observing.
type EventSink func(Event)

// SetEventSink registers the sink that receives every Event, replacing any previous one.
// Passing nil unsubscribes. The sink is guarded by its own lock rather than by Smsc.mu so
// emitting an event never contends with the session map on every PDU.
func (smsc *Smsc) SetEventSink(sink EventSink) {
	smsc.sinkMu.Lock()
	defer smsc.sinkMu.Unlock()
	smsc.sink = sink
}

// emitEvent delivers the event to the registered sink. With no sink registered it is a cheap
// no-op: one read lock and a nil check.
func (smsc *Smsc) emitEvent(event Event) {
	smsc.sinkMu.RLock()
	sink := smsc.sink
	smsc.sinkMu.RUnlock()

	if sink == nil {
		return
	}
	if event.At.IsZero() {
		event.At = time.Now()
	}
	sink(event)
}

// emitPduEvent reads the command_id and sequence_number straight out of the PDU header, so the
// event always reports the bytes that were actually read or written.
func (smsc *Smsc) emitPduEvent(kind EventKind, systemId string, pdu []byte, detail string) {
	if len(pdu) < 16 {
		return
	}
	smsc.emitEvent(Event{
		At:          time.Now(),
		Kind:        kind,
		SystemId:    systemId,
		CommandId:   binary.BigEndian.Uint32(pdu[4:]),
		SequenceNum: binary.BigEndian.Uint32(pdu[12:]),
		Detail:      detail,
	})
}

// BindType is the SMPP bind flavour a session was created with. ReceiveMo only says whether MO
// messages can be delivered, which collapses transmitter and receiver into the same answer.
type BindType string

const (
	BindUnknown     BindType = "unknown"
	BindTransmitter BindType = "transmitter"
	BindReceiver    BindType = "receiver"
	BindTransceiver BindType = "transceiver"
)

// bindTypeForCommandId maps a bind request command_id to its bind type.
func bindTypeForCommandId(cmdId uint32) BindType {
	switch cmdId {
	case BIND_TRANSMITTER:
		return BindTransmitter
	case BIND_RECEIVER:
		return BindReceiver
	case BIND_TRANSCEIVER:
		return BindTransceiver
	default:
		return BindUnknown
	}
}

// SessionInfo is a read-only view of one bound session. It deliberately carries no net.Conn, so
// a subscriber cannot write to the socket behind the simulator's back.
type SessionInfo struct {
	SessionId  int
	SystemId   string
	BindType   BindType
	BoundAt    time.Time
	RemoteAddr string
	ReceiveMo  bool
}

// SessionSnapshot returns the metadata of every live session, one entry per session and sorted
// so a view renders stably. It is safe to call from another goroutine.
//
// BoundSystemIds keeps its existing behaviour, duplicates included, because callers depend on
// it; this is the view built for display.
func (smsc *Smsc) SessionSnapshot() []SessionInfo {
	smsc.mu.RLock()
	infos := make([]SessionInfo, 0, len(smsc.sessions))
	for sessionId, sess := range smsc.sessions {
		infos = append(infos, SessionInfo{
			SessionId:  sessionId,
			SystemId:   sess.SystemId,
			BindType:   sess.BindType,
			BoundAt:    sess.BoundAt,
			RemoteAddr: sess.RemoteAddr,
			ReceiveMo:  sess.ReceiveMo,
		})
	}
	smsc.mu.RUnlock()

	sort.Slice(infos, func(i, j int) bool {
		if infos[i].SystemId != infos[j].SystemId {
			return infos[i].SystemId < infos[j].SystemId
		}
		return infos[i].SessionId < infos[j].SessionId
	})

	return infos
}
