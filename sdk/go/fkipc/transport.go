package fkipc

import (
	"errors"
	"net"
	"time"
)

// Transport is the datagram seam.
//
// It exists so the conformance suite can drive this state machine and the
// guest's against each other over an in-memory link with an injectable fault
// model, in one process, with no sockets and no real time. The default is a
// plain UDP socket; nothing above this interface knows which it has.
type Transport interface {
	// Send puts one datagram on the wire.
	Send(p []byte) error
	// Recv blocks for the next datagram, or returns an error when the
	// transport is closed. The returned slice belongs to the caller.
	Recv() ([]byte, error)
	// Poll returns any datagram already available without blocking, or
	// (nil, false). Manual-mode sessions drive the session from this.
	Poll() ([]byte, bool)
	Close() error
}

// ErrClosed is what Recv returns after Close.
var ErrClosed = errors.New("fkipc: transport closed")

// udpTransport is one bound socket: ListenPort in, 127.0.0.1:GamePort out.
//
// Localhost only, deliberately. The game will not send anywhere else --
// send_udp's own description says "a UDP port on localhost" -- so binding
// anything wider would only widen who can talk to this process.
type udpTransport struct {
	c   *net.UDPConn
	dst *net.UDPAddr
	buf []byte
}

func dialUDP(listen, game uint16) (*udpTransport, error) {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(listen)})
	if err != nil {
		return nil, err
	}
	return &udpTransport{
		c:   c,
		dst: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(game)},
		// One frame is one datagram, and the protocol ceiling is well under
		// any OS limit, so a fixed read buffer with headroom cannot truncate a
		// frame this protocol produced. Anything larger is not ours.
		buf: make([]byte, 16384),
	}, nil
}

func (t *udpTransport) Send(p []byte) error {
	_, err := t.c.WriteToUDP(p, t.dst)
	return err
}

func (t *udpTransport) Recv() ([]byte, error) {
	t.c.SetReadDeadline(time.Time{})
	n, err := t.c.Read(t.buf)
	if err != nil {
		return nil, err
	}
	out := make([]byte, n)
	copy(out, t.buf[:n])
	return out, nil
}

// pollWindow is a SHORT WAIT rather than zero, and the difference is not
// cosmetic: a deadline already in the past makes Read fail immediately without
// looking at what is buffered, so a zero-window poll on a socket with a
// datagram waiting reports nothing. A manual-mode session driven by Pump would
// then never receive anything, on a socket that was working perfectly.
const pollWindow = time.Millisecond

func (t *udpTransport) Poll() ([]byte, bool) {
	t.c.SetReadDeadline(time.Now().Add(pollWindow))
	n, err := t.c.Read(t.buf)
	if err != nil {
		return nil, false
	}
	out := make([]byte, n)
	copy(out, t.buf[:n])
	return out, true
}

func (t *udpTransport) Close() error { return t.c.Close() }
