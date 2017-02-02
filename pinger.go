package ping

import (
	"errors"
	"log"
	"net"
	"os"
	"time"

	"sync"

	"sync/atomic"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

const (
	ProtocolICMP     = 1  // Internet Control Message
	ProtocolIPv6ICMP = 58 // ICMP for IPv6
)

// sequence number for this process
var sequence uint32

// Pinger is a instance for ICMP echo requests
type Pinger struct {
	Local    net.IP              // IP address to bind on
	Attempts uint                // number of attempts
	Timeout  time.Duration       // timeout per request
	requests map[uint16]*request // running requests
	mtx      sync.Mutex          // lock for the requests map
	id       uint16
	conn     *icmp.PacketConn
	wg       sync.WaitGroup
}

type request struct {
	wait   chan struct{}
	result error
}

// schreibt einen Antwort und schließt den Channel
func (req *request) respond(err error) {
	req.result = err
	close(req.wait)
}

// New creates a new Pinger
func New(bind string) (*Pinger, error) {
	// Socket öffnen
	conn, err := icmp.ListenPacket("ip4:icmp", bind)
	if err != nil {
		return nil, err
	}

	pinger := Pinger{
		conn:     conn,
		id:       uint16(os.Getpid()),
		requests: make(map[uint16]*request),
	}

	pinger.wg.Add(1)
	go pinger.receiver()

	return &pinger, nil
}

// Close schließt den Socket
func (pinger *Pinger) Close() {
	pinger.conn.Close()
	pinger.wg.Wait()
}

// Ping sendet ICMP echo requests bis einer erfolgreich ist, oder die Versuche ausgeschöpft sind
func (pinger *Pinger) Ping(remote net.Addr) (err error) {
	// Mehrere Versuche
	for i := uint(0); i < pinger.Attempts; i++ {
		// Timeout setzen
		pinger.conn.SetDeadline(time.Now().Add(pinger.Timeout))

		// Pingen
		if err = pinger.once(remote); err == nil {
			// erfolgreich
			break
		}
	}

	return
}

// Schickt einen Ping ab und wartet auf Antwort
func (pinger *Pinger) once(remote net.Addr) error {
	seq := uint16(atomic.AddUint32(&sequence, 1))
	req := request{
		wait: make(chan struct{}),
	}

	// Paket bauen
	wm := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{
			ID:  int(pinger.id),
			Seq: int(seq),
		},
	}
	// Paket serialisieren
	wb, err := wm.Marshal(nil)
	if err != nil {
		return err
	}

	// In die laufenden Anfragen eintragen
	pinger.mtx.Lock()
	pinger.requests[seq] = &req
	pinger.mtx.Unlock()

	// Anfrage abschicken
	if _, err := pinger.conn.WriteTo(wb, remote); err != nil {
		req.respond(err)
	}

	// Auf Antwort warten
	select {
	case <-req.wait:
		err = req.result
	case <-time.After(pinger.Timeout):
		err = errors.New("timeout")
	}

	// Aus den laufenden Anfragen entfernen
	pinger.mtx.Lock()
	delete(pinger.requests, seq)
	pinger.mtx.Unlock()

	return err
}

// reads the incoming ICMP packets
func (pinger *Pinger) receiver() {
	rb := make([]byte, 1500)

	// Read imcoming packets
	for {
		if n, _, err := pinger.conn.ReadFrom(rb); err != nil {
			break // socket gone
		} else {
			pinger.receive(rb[:n])
		}
	}

	// Close running requests
	pinger.mtx.Lock()
	for _, req := range pinger.requests {
		req.respond(errors.New("pinger closed"))
	}
	pinger.mtx.Unlock()

	// Notify Close() method
	pinger.wg.Done()
}

// Processes a ICMP packet
func (pinger *Pinger) receive(bytes []byte) {
	// Antwort parsen
	rm, err := icmp.ParseMessage(ProtocolICMP, bytes)
	if err != nil {
		log.Println(err)
		return
	}

	// Antwort auswerten
	switch rm.Type {
	case ipv4.ICMPTypeEchoReply:
		body := rm.Body.(*icmp.Echo)
		if body == nil {
			return
		}

		// Check ID field
		if uint16(body.ID) != pinger.id {
			return
		}

		// Search for existing running echo request
		pinger.mtx.Lock()
		if req := pinger.requests[uint16(body.Seq)]; req != nil {
			req.respond(nil)
		}
		pinger.mtx.Unlock()
	case ipv4.ICMPTypeDestinationUnreachable:
		body := rm.Body.(*icmp.DstUnreach)
		if body == nil {
			return
		}
		// TODO parse data
	default:
		// other ICMP packet
		log.Printf("got: %+v %d", rm, rm.Body.Len(1))
	}
}