package main

import (
	"io"
	"io/ioutil"
	"net"
	"testing"
)

// a client that binds a transmitter and a receiver under one system_id must get its MO
// messages on the receiver. picking the first match out of the map made this a coin flip,
// because go randomizes map iteration order
func TestMoMessagesGoToASessionThatCanReceiveThem(t *testing.T) {
	smsc := NewSmsc(false)

	transmitter, closeTransmitter := drainedPipe()
	defer closeTransmitter()

	receiver, closeReceiver := drainedPipe()
	defer closeReceiver()

	smsc.Sessions[1] = Session{SystemId: "client1", Conn: transmitter, ReceiveMo: false}
	smsc.Sessions[2] = Session{SystemId: "client1", Conn: receiver, ReceiveMo: true}

	for attempt := 1; attempt <= 50; attempt++ {
		if err := smsc.SendMoMessage("77012110000", "1001", "Test", "client1"); err != nil {
			t.Fatalf("attempt %d of 50 was refused: %v. a session able to receive MO was bound under this system_id the whole time", attempt, err)
		}
	}
}

// preferring a receiver must not become "deliver to anything"
func TestMoMessagesAreStillRefusedWhenNoSessionCanReceiveThem(t *testing.T) {
	smsc := NewSmsc(false)

	transmitter, closeTransmitter := drainedPipe()
	defer closeTransmitter()

	smsc.Sessions[1] = Session{SystemId: "client1", Conn: transmitter, ReceiveMo: false}

	if err := smsc.SendMoMessage("77012110000", "1001", "Test", "client1"); err == nil {
		t.Errorf("a client with only a transmitter bound cannot receive an MO message")
	}
}

// net.Pipe is unbuffered, so the far end has to be drained or a write blocks forever
func drainedPipe() (net.Conn, func()) {
	near, far := net.Pipe()

	drained := make(chan struct{})
	go func() {
		io.Copy(ioutil.Discard, far)
		close(drained)
	}()

	return near, func() {
		near.Close()
		far.Close()
		<-drained
	}
}
