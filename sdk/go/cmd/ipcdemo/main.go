// Command ipcdemo drives two fkipc mods in ONE running Factorio at once: a Go
// mod that drags the sun and a Rust mod that resizes a rendered circle, both
// steered live from a local web page and both streaming telemetry back.
//
// It is the worked example for the thing sdk/go/cmd/ipcgate cannot show, which
// is MULTIPLEXING. --enable-lua-udp binds ONE socket for the whole game, so
// every inbound datagram raises on_udp_packet_received in EVERY mod: two mods
// in one game is not two independent conversations by default, it is one wire
// both of them hear. Each side is scoped differently and it is worth stating
// which is which, because only one of them needed building:
//
//   - OUT HERE, the scoping is free and structural. Each Session binds its own
//     socket, and the game sends to a DESTINATION port that is the mod's own
//     Config.Port, so a datagram meant for the daylight mod's companion is
//     never delivered to the circle mod's. That is the operating system, not
//     this program. TestTwoSessionsDoNotSeeEachOthersTraffic pins it.
//
//   - IN THERE, nothing is scoped and the library had to do it. Both guests are
//     handed both companions' frames. The epoch filter catches almost all of
//     it, and the hole it cannot catch is HELLO_ACK, which is matched on corr
//     with the epoch test skipped because it carries an epoch the guest does
//     not yet know -- and corr comes from a counter, so two freshly-loaded
//     guests both send their first HELLO with corr = 1. The fix is the
//     source-port filter in guest/{go,rust}/fkipc, and -smoke's foreign-port
//     leg is what proves it against a real game.
//
// Two modes:
//
//	ipcdemo                 # serve http://localhost:8080 and hold both sessions
//	ipcdemo -smoke          # no HTTP: a scripted run with PASS/FAIL legs
//
// The -smoke transcript is the same shape as ipcgate's -- one
// `PASS <leg> -- detail` or `FAIL <leg> -- detail` per leg, then `RESULT ok` or
// `RESULT failed N`. The first two fields are the run-to-run comparable part
// and the detail deliberately is not: the epoch is entropy this side minted and
// the tick a datagram lands on is a race between a real clock and the game's
// update loop.
//
// The three ports are flags with defaults, but the two MOD ports are compiled
// into the guests -- a guest has no configuration file -- so changing one here
// without changing it there gives a companion listening to nobody.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	// requestTimeout is one RPC's deadline. Round-trip latency through the
	// InputAction path is median 31.5 ms and p90 94.8 ms on a headless server,
	// and the guest's own retry schedule runs to about 2.8 s, so this has to
	// clear the retries rather than the latency.
	requestTimeout = 5 * time.Second
)

func main() {
	gamePort := flag.Uint("game-port", 29433,
		"--enable-lua-udp's port: the game's ONE socket, shared by every mod")
	daylightPort := flag.Uint("daylight-port", 29434,
		"our listen port for the Go daylight mod; compiled into the guest")
	circlePort := flag.Uint("circle-port", 29437,
		"our listen port for the Rust circle mod; compiled into the guest")
	httpAddr := flag.String("http", ":8080", "where to serve the control page")
	smoke := flag.Bool("smoke", false,
		"no HTTP: run the scripted PASS/FAIL conversation and exit")
	timeout := flag.Duration("timeout", 120*time.Second,
		"-smoke only: the whole run's deadline")
	step := flag.Duration("step", 25*time.Second, "-smoke only: one leg's deadline")
	verbose := flag.Bool("v", false, "log the SDK's own diagnostics")
	flag.Parse()

	if err := checkPorts(*gamePort, *daylightPort, *circlePort); err != nil {
		fmt.Printf("FAIL ports -- %v\n", err)
		fmt.Println("RESULT failed 1")
		os.Exit(2)
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	lg := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	hub := newHub()
	var links []*link
	for _, spec := range specs(uint16(*daylightPort), uint16(*circlePort)) {
		l, err := newLink(spec, uint16(*gamePort), lg, *smoke, hub.notify)
		if err != nil {
			fmt.Printf("FAIL dial-%s -- %v\n", spec.Key, err)
			fmt.Println("RESULT failed 1")
			closeAll(links)
			os.Exit(2)
		}
		links = append(links, l)
	}
	defer closeAll(links)

	if *smoke {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()
		if runSmoke(ctx, lg, links, uint16(*gamePort), *step) != 0 {
			closeAll(links)
			os.Exit(1)
		}
		return
	}

	serve(lg, hub, links, *httpAddr, uint16(*gamePort))
}

// checkPorts refuses the two mistakes that produce a session which never
// receives anything and says nothing about why.
func checkPorts(game, daylight, circle uint) error {
	if daylight == game || circle == game {
		return fmt.Errorf("a mod port equals -game-port (%d): --enable-lua-udp "+
			"binds ONE socket and it is also the SOURCE port of everything the "+
			"game sends, so a companion sharing it is the game talking to itself",
			game)
	}
	if daylight == circle {
		return fmt.Errorf("-daylight-port and -circle-port are both %d: the "+
			"DESTINATION port is the only thing that routes a frame to the right "+
			"companion, and the guests' source-port filters would refuse each "+
			"other's traffic anyway", daylight)
	}
	return nil
}

func closeAll(links []*link) {
	for _, l := range links {
		if l != nil && l.sess != nil {
			// Close says BYE, which is advisory -- the guest recovers from this
			// side simply vanishing, by liveness -- but it turns a three-second
			// timeout into an immediate one, and it is the guest-side half that
			// run-ipcdemo.sh reads out of the game's log.
			l.sess.Close()
		}
	}
}

// waitForSignal blocks until the user asks to stop. The graphical mode has no
// deadline of its own: a person dragging sliders is the workload.
func waitForSignal() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
}
