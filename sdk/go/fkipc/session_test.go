package fkipc_test

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	guestipc "github.com/Techrocket9/fklua/guest/go/fkipc"
	"github.com/Techrocket9/fklua/guest/go/fkipc/wire"
	sdkipc "github.com/Techrocket9/fklua/sdk/go/fkipc"
)

// Dial refuses ListenPort == GamePort.
//
// --enable-lua-udp binds ONE socket, and it is both the game's receive socket
// and the source port of everything the game sends. A companion sharing it is
// the game talking to itself, and the failure without this check is a session
// that never receives anything and says nothing about why.
func TestDialRefusesTheGamesOwnPort(t *testing.T) {
	_, err := sdkipc.Dial(sdkipc.Options{GamePort: 29433, ListenPort: 29433})
	if err == nil {
		t.Fatal("Dial accepted the game's own port")
	}
	if !strings.Contains(err.Error(), "ONE socket") {
		t.Errorf("the refusal does not explain itself: %v", err)
	}
	if _, err := sdkipc.Dial(sdkipc.Options{ListenPort: 29434}); err == nil {
		t.Error("Dial accepted a missing GamePort")
	}
}

// A real socket pair, because everything else in this package's tests runs over
// an in-memory link and the one thing that cannot say is whether the datagram
// path is wired up at all.
func TestARealSocketCarriesAFrame(t *testing.T) {
	a, err := sdkipc.Dial(sdkipc.Options{GamePort: 45677, ListenPort: 45676, Manual: true})
	if err != nil {
		t.Skipf("cannot bind a loopback UDP port here: %v", err)
	}
	defer a.Close()

	// Driven from a bare socket rather than a second Session, because a Session
	// with no epoch could not send anything the first one would accept -- and
	// what is under test is the datagram path, not the protocol.
	c, err := net.Dial("udp4", "127.0.0.1:45676")
	if err != nil {
		t.Skipf("cannot dial loopback: %v", err)
	}
	defer c.Close()

	hello, err := wire.AppendHello(nil, wire.Hello{ProtoMin: 1, ProtoMax: 1,
		MaxFrame: wire.DefaultMaxFrame, MaxFragments: wire.MaxFragments, Name: "sock"})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := wire.AppendFrame(nil, wire.Header{Type: wire.TypeHello, Corr: 1}, hello)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(frame); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		a.Pump()
		if a.Stats().Sessions > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("a HELLO sent over a real loopback socket never arrived")
}

// File pickup: the digested case is EXACT and the digest-less case
// stabilize-polls, and the difference is not decoration.
//
// A file the guest wrote it also hashed, so "ready" is a fact. A file the
// ENGINE wrote -- a screenshot -- the guest never held and cannot describe, so
// "ready" is a heuristic, and it is labelled as one because nothing documents a
// flush guarantee for write_file in either direction.
func TestFilePickupWaitsForTheDigestAndFallsBackToStability(t *testing.T) {
	dir := t.TempDir()
	// THE PATH IS CONFIGURATION WITH A DEFAULT, which is the whole reason
	// Options.ScriptOutput exists: an SDK a downstream author points at their
	// own install must take the directory rather than guess three ways.
	h := newHarness(t, opts{scriptOutput: dir})
	h.up()

	got := make(chan []byte, 4)
	h.s.OnFile(func(n sdkipc.FileNotify, r io.ReadCloser) {
		b, _ := io.ReadAll(r)
		r.Close()
		got <- b
	})
	c := h.g.Chan(1, guestipc.PriBulk)

	// The digested leg. The bytes appear a moment AFTER the notify, which is
	// exactly the case the digest exists for -- nothing documents a flush
	// guarantee, so the notify is not "the bytes are all there".
	body := bytes.Repeat([]byte("bulk"), 300)
	go func() {
		time.Sleep(120 * time.Millisecond)
		os.WriteFile(filepath.Join(dir, "dump.bin"), body, 0o644)
	}()
	if st := guestipc.WriteBulk(c, "dump.bin", body); st != guestipc.StatusOK {
		t.Fatalf("WriteBulk: %v", st)
	}
	h.step(3)
	select {
	case b := <-got:
		if !bytes.Equal(b, body) {
			t.Errorf("digested pickup returned %d bytes, want %d", len(b), len(body))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the digested file was never picked up")
	}

	// The digest-less leg: a file the guest never held, so the peer can only
	// wait for the size to stop moving. A heuristic, labelled as one.
	os.WriteFile(filepath.Join(dir, "shot.png"), []byte("PNGDATA"), 0o644)
	if st := guestipc.NotifyFile(c, "shot.png"); st != guestipc.StatusOK {
		t.Fatalf("NotifyFile: %v", st)
	}
	h.step(3)
	select {
	case b := <-got:
		if string(b) != "PNGDATA" {
			t.Errorf("stabilize pickup returned %q", b)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the digest-less file was never picked up")
	}
}

func TestDefaultScriptOutputHonoursTheUserDir(t *testing.T) {
	t.Setenv("FACTORIO_USERDIR", filepath.Join("tmp", "fkuser"))
	got, err := sdkipc.DefaultScriptOutput()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("tmp", "fkuser", "script-output")
	if got != want {
		t.Errorf("got %q, want %q -- FACTORIO_USERDIR is what this repo's own "+
			"in-game harnesses set, and a headless run under one writes nowhere "+
			"near the platform default", got, want)
	}
	t.Setenv("FACTORIO_USERDIR", "")
	if _, err := sdkipc.DefaultScriptOutput(); err != nil {
		t.Errorf("the platform default: %v", err)
	}
}
