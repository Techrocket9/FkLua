// Package fkipc is the external half of the fkipc link: a companion process on
// the same machine as Factorio, talking to a guest built against
// guest/go/fkipc.
//
//	s, err := fkipc.Dial(fkipc.Options{GamePort: 29433, ListenPort: 29434})
//	s.Subscribe(1, func(m fkipc.Message) { ... })
//	out, err := s.Request(ctx, 2, []byte("ping"))
//
// The game is started with `factorio --enable-lua-udp 29433`.
//
// # The two ends are NOT mirrors, and that is deliberate: one of them has a clock
//
// This side may block, may use contexts, may use goroutines and keeps its
// timers in real time. Forcing it into the guest's callback shape would be
// cargo-culting a constraint that does not apply -- the guest has no wall
// clock, no scheduler and a heap that is in the save, and none of those is true
// here.
//
// What it does share is the codec, which lives in the GUEST module at
// guest/go/fkipc/wire and is imported from here. One codec, two consumers.
//
// # Three things this side owns because the guest cannot
//
// THE CLOCK. Retry deadlines, the quiet-peer throttle and the stabilize-poll
// for a digest-less file are all real time here. The guest counts ticks, and
// the two are reconciled by the tick every HEARTBEAT carries.
//
// FILE PICKUP. [Session.OnFile] waits for a file to satisfy its notify --
// exact length and checksum when the guest wrote it, size-stable across two
// polls when the engine did -- and hands the caller an open reader. THE PATH IS
// CONFIGURATION WITH A DEFAULT, not a guess: an SDK a downstream author points
// at their own install must take the directory and only fall back to
// [DefaultScriptOutput].
//
// THE PORT CHECK. `--enable-lua-udp` binds ONE socket, which is both the game's
// receive socket and the source port of everything it sends. A companion on
// that same port is not a subtle bug -- it is the game talking to itself -- so
// [Dial] refuses rather than producing a session that never receives anything.
//
// # Minting the epoch, which is this side's real job
//
// THE GUEST CANNOT MINT A UNIQUE SESSION ID, and that is a theorem rather than
// a limitation to work around: everything a guest can compute is a
// deterministic function of its own state, and its own state time-travels. Load
// a save twice and the guest computes the same value both times, by
// construction. So the uniqueness comes from the side that has entropy, which
// is this one.
//
// A HELLO is therefore ALWAYS a new session. This side does not compare the
// guest's boot counter -- boot aliases across two loads of one save, and a peer
// that trusted it would carry state across a boundary the guest has already
// forgotten. THE HELLO IS THE SESSION BOUNDARY AND THE EPOCH IS THE FRAME
// FILTER: two jobs, two mechanisms.
package fkipc
