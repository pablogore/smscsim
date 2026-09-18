package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// the session store is already guarded by a mutex, but the store is still reachable
// without it, entries outlive their bind, and the receipt goroutine reads a variable the
// read loop rewrites. these tests drive the real server and are meant to run with -race

func TestConcurrentBindChurnIsRaceFree(t *testing.T) {
	smsc := NewSmsc(false)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot listen: %v", err)
	}
	defer listener.Close()

	go smsc.serve(listener)

	address := listener.Addr().String()

	const clients = 24
	const rounds = 12

	var wg sync.WaitGroup

	for c := 0; c < clients; c++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			systemId := fmt.Sprintf("client%d", id)
			for r := 0; r < rounds; r++ {
				bindUnbind(t, address, systemId)
			}
		}(c)
	}

	// read the store the way the web page does while the churn runs. the MO target is
	// never bound on purpose, so no deliver_sm can desynchronize a client's pdu stream
	done := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-done:
					return
				default:
					smsc.BoundSystemIds()
					smsc.SendMoMessage("77012110000", "1001", "Test", "unbound")
					time.Sleep(time.Millisecond)
				}
			}
		}()
	}

	wg.Wait()
	close(done)
	readers.Wait()
}

// an unbound session used to stay in the store, so the system_id was still offered by
// the web page and MO messages still selected a connection on its way out
func TestUnbindRemovesTheSessionFromTheStore(t *testing.T) {
	smsc := NewSmsc(false)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot listen: %v", err)
	}
	defer listener.Close()

	go smsc.serve(listener)

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("cannot dial: %v", err)
	}
	defer conn.Close()

	writePDU(t, conn, stringBodyPDU(BIND_TRANSCEIVER, STS_OK, 1, "client1"))
	readPDU(t, conn)

	if !awaitBound(smsc, "client1", true) {
		t.Fatalf("bind was not registered, so this test says nothing about the unbind")
	}

	writePDU(t, conn, headerPDU(UNBIND, STS_OK, 2))
	readPDU(t, conn)

	if !awaitBound(smsc, "client1", false) {
		t.Errorf("unbound session must not remain in the store, BoundSystemIds reports %v", smsc.BoundSystemIds())
	}
}

// the delivery receipt goroutine reads the system_id variable the read loop keeps rewriting,
// so a receipt scheduled by one bind is reported under whichever system_id is bound two
// seconds later. submit with registered_delivery, then unbind and rebind while it sleeps
func TestDeliveryReceiptIsReportedUnderTheSubmittingSystemId(t *testing.T) {
	logs := captureLog(t)

	smsc := NewSmsc(false)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot listen: %v", err)
	}
	defer listener.Close()

	go smsc.serve(listener)

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("cannot dial: %v", err)
	}
	defer conn.Close()

	writePDU(t, conn, stringBodyPDU(BIND_TRANSCEIVER, STS_OK, 1, "submitter"))
	readPDU(t, conn)

	writePDU(t, conn, submitSmPDU(2, "1001", "77012110000", "Test", 1))

	writePDU(t, conn, headerPDU(UNBIND, STS_OK, 3))
	writePDU(t, conn, stringBodyPDU(BIND_TRANSCEIVER, STS_OK, 4, "rebound"))

	if !awaitDeliverSm(t, conn) {
		t.Fatalf("delivery receipt scheduled before a rebind must still be delivered on the same connection")
	}

	receipt := awaitLogLine(logs, "delivery receipt for message")
	if receipt == "" {
		t.Fatalf("expected the simulator to log the delivery receipt it just sent")
	}
	if strings.Contains(receipt, "rebound") {
		t.Errorf("receipt attributed to the system_id bound afterwards, not the one that submitted: %s", receipt)
	}
	if !strings.Contains(receipt, "submitter") {
		t.Errorf("expected the receipt to name the submitting system_id, got: %s", receipt)
	}
}

// helpers

// log output is written from the receipt goroutine while the test reads it
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLog(t *testing.T) *syncBuffer {
	buf := &syncBuffer{}
	previous := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(previous) })
	return buf
}

func awaitLogLine(logs *syncBuffer, needle string) string {
	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		for _, line := range strings.Split(logs.String(), "\n") {
			if strings.Contains(line, needle) {
				return line
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	return ""
}

func bindUnbind(t *testing.T, address, systemId string) {
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Errorf("cannot dial %s: %v", address, err)
		return
	}
	defer conn.Close()

	writePDU(t, conn, stringBodyPDU(BIND_TRANSCEIVER, STS_OK, 1, systemId))
	readPDU(t, conn)

	writePDU(t, conn, headerPDU(ENQUIRE_LINK, STS_OK, 2))
	readPDU(t, conn)

	writePDU(t, conn, headerPDU(UNBIND, STS_OK, 3))
	readPDU(t, conn)
}

func writePDU(t *testing.T, conn net.Conn, pdu []byte) {
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(pdu); err != nil {
		t.Errorf("cannot write pdu: %v", err)
	}
}

// reads one whole pdu, so the next read starts on a header boundary
func readPDU(t *testing.T, conn net.Conn) []byte {
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	head := make([]byte, 16)
	if _, err := io.ReadFull(conn, head); err != nil {
		t.Errorf("cannot read pdu header: %v", err)
		return nil
	}

	cmdLen := binary.BigEndian.Uint32(head[0:])
	if cmdLen < 16 {
		t.Errorf("pdu declares a command_length of %d, shorter than its own header", cmdLen)
		return nil
	}
	if cmdLen == 16 {
		return head
	}

	body := make([]byte, cmdLen-16)
	if _, err := io.ReadFull(conn, body); err != nil {
		t.Errorf("cannot read pdu body: %v", err)
		return nil
	}

	return append(head, body...)
}

// the handler registers on its own goroutine, so the state change is polled, not assumed
func awaitBound(smsc *Smsc, systemId string, want bool) bool {
	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		found := false
		for _, id := range smsc.BoundSystemIds() {
			if id == systemId {
				found = true
				break
			}
		}
		if found == want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}

	return false
}

// unbind_resp and bind_resp arrive first, so the receipt cannot be found by reading a
// fixed number of pdus
func awaitDeliverSm(t *testing.T, conn net.Conn) bool {
	deadline := time.Now().Add(15 * time.Second)

	for time.Now().Before(deadline) {
		conn.SetReadDeadline(deadline)

		head := make([]byte, 16)
		if _, err := io.ReadFull(conn, head); err != nil {
			return false
		}

		cmdLen := binary.BigEndian.Uint32(head[0:])
		cmdId := binary.BigEndian.Uint32(head[4:])

		if cmdLen > 16 {
			body := make([]byte, cmdLen-16)
			if _, err := io.ReadFull(conn, body); err != nil {
				return false
			}
		}

		if cmdId == DELIVER_SM {
			return true
		}
		if cmdId == GENERIC_NACK {
			t.Errorf("simulator rejected the submit_sm, so no receipt was ever scheduled")
			return false
		}
	}

	return false
}

// builds a submit_sm the simulator's own parser accepts. it walks the body by adding
// fixed strides to each null terminator, so the empty optional strings matter
func submitSmPDU(seqNum uint32, srcAddr, destAddr, message string, registeredDelivery byte) []byte {
	var body bytes.Buffer

	body.WriteByte(0) // service_type, empty

	body.WriteByte(0) // source_addr_ton
	body.WriteByte(0) // source_addr_npi
	body.WriteString(srcAddr)
	body.WriteByte(0)

	body.WriteByte(0) // dest_addr_ton
	body.WriteByte(0) // dest_addr_npi
	body.WriteString(destAddr)
	body.WriteByte(0)

	body.WriteByte(0) // esm_class
	body.WriteByte(0) // protocol_id
	body.WriteByte(0) // priority_flag

	body.WriteByte(0) // schedule_delivery_time, empty
	body.WriteByte(0) // validity_period, empty

	body.WriteByte(registeredDelivery)
	body.WriteByte(0) // replace_if_present_flag
	body.WriteByte(0) // data_coding
	body.WriteByte(0) // sm_default_msg_id

	body.WriteByte(byte(len(message)))
	body.WriteString(message)

	cmdLen := 16 + body.Len()

	var pdu bytes.Buffer
	head := make([]byte, 16)
	binary.BigEndian.PutUint32(head[0:], uint32(cmdLen))
	binary.BigEndian.PutUint32(head[4:], SUBMIT_SM)
	binary.BigEndian.PutUint32(head[8:], STS_OK)
	binary.BigEndian.PutUint32(head[12:], seqNum)
	pdu.Write(head)
	pdu.Write(body.Bytes())

	return pdu.Bytes()
}
