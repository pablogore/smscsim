package main

import (
	"net"
	"sync"
	"testing"
)

// Regression test for the "concurrent map writes" crash: two smpp clients
// binding at the same time used to write smsc.Sessions without any lock.
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
			smsc.addSession(sessionId, Session{"system_id", server, true})
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

	smsc.addSession(1, Session{"transceiver", server, true})

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
