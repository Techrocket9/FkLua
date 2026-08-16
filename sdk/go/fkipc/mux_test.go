package fkipc_test

import (
	"net"
	"testing"
	"time"

	"github.com/Techrocket9/fklua/guest/go/fkipc/wire"
	sdkipc "github.com/Techrocket9/fklua/sdk/go/fkipc"
)

// TWO IPC MODS IN ONE GAME, FROM THIS SIDE OF THE WIRE.
//
// The guest side of that arrangement needed real work: --enable-lua-udp binds
// ONE socket for the whole game, so on_udp_packet_received fires in every mod
// for every mod's datagrams and each guest link has to filter on the sender's
// port (guest/go/fkipc's Link.deliver and its Rust mirror).
//
// OUT HERE THE SCOPING IS FREE AND STRUCTURAL, and this test is that claim
// written down rather than assumed -- "it cannot happen" is exactly how the
// hole on the other side survived. Each Session binds its OWN socket, and the
// game addresses each mod's frames to that mod's Config.Port, so the routing is
// the operating system's and no companion can be reached by another companion's
// mod. A HELLO delivered to one listen port must bring up exactly one session.
//
// It uses REAL SOCKETS, deliberately: an in-memory transport would be a model
// of the very thing under test.
func TestTwoSessionsDoNotSeeEachOthersTraffic(t *testing.T) {
	gameConn := mustBindUDP(t)
	defer gameConn.Close()
	gamePort := uint16(gameConn.LocalAddr().(*net.UDPAddr).Port)
	portA, portB := freePort(t), freePort(t)

	a := mustDial(t, gamePort, portA, "A")
	defer a.Close()
	b := mustDial(t, gamePort, portB, "B")
	defer b.Close()

	// The game speaks to mod A's companion only. Same source socket for both
	// mods -- that is the whole point -- and a different DESTINATION.
	sendFrom(t, gameConn, portA, hello(t, 7, "mod-a"))

	if !waitUp(a, 2*time.Second) {
		t.Fatal("the session addressed by destination port never came up")
	}
	if b.Stats().Up {
		t.Fatal("a HELLO sent to one companion's port brought up the OTHER " +
			"companion's session: the destination port is the only thing " +
			"separating two mods' conversations on this side")
	}
	if n := b.Stats().RxFrames + b.Stats().Drops; n != 0 {
		t.Errorf("the second session saw %d frames it was not addressed", n)
	}

	// ...and the other way round, so this is not an artefact of which one was
	// dialled first.
	sendFrom(t, gameConn, portB, hello(t, 9, "mod-b"))
	if !waitUp(b, 2*time.Second) {
		t.Fatal("the second session never came up on its own port")
	}
	if a.Stats().Sessions != 1 {
		t.Errorf("session A minted %d sessions; the second mod's HELLO reached it",
			a.Stats().Sessions)
	}
	if a.Stats().Epoch == b.Stats().Epoch {
		t.Error("both sessions minted the same epoch -- the token is drawn from " +
			"real entropy precisely so two sessions cannot collide")
	}
}

func mustDial(t *testing.T, gamePort, listenPort uint16, name string) *sdkipc.Session {
	t.Helper()
	s, err := sdkipc.Dial(sdkipc.Options{
		GamePort: gamePort, ListenPort: listenPort, Name: name,
	})
	if err != nil {
		t.Fatalf("Dial %s: %v", name, err)
	}
	return s
}

func mustBindUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// freePort takes a port the kernel just handed out and gives it straight back.
// It is a race in principle and the standard one; the alternative is a fixed
// number, which fails whenever a previous run's socket is still in TIME_WAIT.
func freePort(t *testing.T) uint16 {
	t.Helper()
	c := mustBindUDP(t)
	p := uint16(c.LocalAddr().(*net.UDPAddr).Port)
	c.Close()
	return p
}

func sendFrom(t *testing.T, c *net.UDPConn, dst uint16, frame []byte) {
	t.Helper()
	_, err := c.WriteToUDP(frame, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(dst)})
	if err != nil {
		t.Fatal(err)
	}
}

func hello(t *testing.T, boot uint32, name string) []byte {
	t.Helper()
	ctl, err := wire.AppendHello(nil, wire.Hello{
		ProtoMin: wire.Version, ProtoMax: wire.Version,
		MaxFrame: wire.DefaultMaxFrame, MaxFragments: wire.MaxFragments,
		Boot: boot, Name: name,
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := wire.AppendFrame(nil, wire.Header{
		Type: wire.TypeHello, Epoch: boot, Corr: 1,
	}, ctl)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func waitUp(s *sdkipc.Session, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if s.Stats().Up {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
