//go:build !tinygo.wasm

package fkipc

// Off-target, there is no send_udp and no event dispatcher, so there is no
// transport this package can build for itself. What there is instead is
// [Attach], which is the seam that makes the conformance suite a test OF THE
// SHIPPING STATE MACHINE rather than of a second implementation somebody has to
// keep in sync with it.
//
// The precedent is fkgc's own build-tag split, and the reason is the same:
// everything above the transport -- the handshake, the epoch filter, dedup,
// serial arithmetic, reassembly, retries, the quiesce -- is ordinary Go with no
// dependency on the target, and it is exactly the part two implementations get
// wrong.

func newTransport(cfg Config) (Transport, Status) { return nil, StatusNoTransport }

// Attach builds an independent link over a caller-supplied transport.
//
// It exists for host-side tests and is not compiled into a guest. A guest has
// one link and reaches it through the package-level Open/Pump/Reload/OnEvent; a
// test wants several, in one process, without a package-level singleton making
// them serial.
func Attach(cfg Config, tr Transport) (*Link, Status) { return newLink(cfg, tr) }

// RestoreBoot models what a save carries.
//
// boot lives in guest memory, which is persisted, and Reload bumps it. TWO
// LOADS OF ONE SAVE THEREFORE PRODUCE THE SAME boot, by construction -- that is
// the theorem the whole epoch design rests on, and it is what a test asserting
// "the peer must not trust boot" needs to be able to set up. Off-target there
// is no linear memory to restore, so the value is handed over instead.
func (l *Link) RestoreBoot(b uint32) {
	if l != nil {
		l.boot = b
		l.stats.Boot = b
	}
}

// Tick reports the last tick handed to Pump, so a test can assert on the
// library's own notion of time rather than on its own loop counter.
func (l *Link) Tick() uint32 {
	if l == nil {
		return 0
	}
	return l.tick
}
