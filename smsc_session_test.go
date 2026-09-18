package main

import (
	"net"
	"sync"
	"testing"
)

// Regression test for the "concurrent map writes" crash: two smpp clients
// binding at the same time used to write the session map without any lock.
func TestConcurrentSessionAccessIsRaceFree(t *testing.T) {
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
}

func TestFindSessionReturnsTheBoundSession(t *testing.T) {
	smsc := NewSmsc(false)
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	smsc.addSession(1, Session{SystemId: "transceiver", Conn: server, ReceiveMo: true})

	sess, found := smsc.findSession("transceiver")
	if !found {
		t.Fatalf("expected to find a session for system_id [transceiver]")
	}
	if !sess.ReceiveMo {
		t.Errorf("expected the bound session to accept MO messages")
	}

	if _, found := smsc.findSession("unknown"); found {
		t.Errorf("expected no session for an unbound system_id")
	}
}

// A transmitter and a receiver normally share one system_id. Map iteration order is randomized, so
// this runs enough times to make a first-match implementation lose.
func TestFindSessionPrefersAnMoCapableBind(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	for i := 0; i < 200; i++ {
		smsc := NewSmsc(false)
		smsc.addSession(1, Session{SystemId: "smppclient", Conn: server, ReceiveMo: false}) // transmitter
		smsc.addSession(2, Session{SystemId: "smppclient", Conn: server, ReceiveMo: true})  // receiver

		sess, found := smsc.findSession("smppclient")
		if !found {
			t.Fatalf("expected to find a session for system_id [smppclient]")
		}
		if !sess.ReceiveMo {
			t.Fatalf("attempt %d picked the transmitter; MO delivery would have been refused", i)
		}
	}
}

func TestFindSessionStillReturnsATransmitterWhenNothingCanCarryMo(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	smsc := NewSmsc(false)
	smsc.addSession(1, Session{SystemId: "smppclient", Conn: server, ReceiveMo: false})

	sess, found := smsc.findSession("smppclient")
	if !found {
		t.Fatalf("expected to find the transmitter session")
	}
	if sess.ReceiveMo {
		t.Errorf("expected the transmitter, so SendMoMessage can refuse it explicitly")
	}
}
