package fkipc

// The engine gate.
//
// THIS IS THE ONE PLACE WHERE GETTING IT WRONG KILLS THE PROCESS, so it is the
// one place that refuses by default. On Factorio 2.0.77 a headless server that
// calls recv_udp with a packet queued aborts in C++ at TickClosure.cpp:91 --
// not a Lua error, so no pcall anywhere on the bridge survives it, and the
// mod's own logging stops mid-tick. The probe reproduced it five times in five
// runs, deterministic to the map tick on a fresh map.
//
// The crash needs BOTH halves: recv_udp on an empty socket is safe (1,500 calls,
// fine) and a socket nobody reads is safe (20 packets piled up, fine). Only the
// pump and a queued packet together do it. It is also specifically reading FOR
// THE SERVER -- recv_udp(0) and bare recv_udp() both crash, recv_udp(1) for a
// player who does not exist is a safe no-op that delivers nothing.
//
// BELOW THE FLOOR THE LIBRARY IS INERT, WHICH IS WIDER THAN THE CRASH. It used
// to run SEND-ONLY down there, on the reasoning that outbound is free and a
// telemetry guest could still be useful with nobody listening. That is true of
// the datagrams and false of the PROTOCOL: a session is established by a
// HELLO_ACK, an ACK arrives inbound, and inbound is the direction that is shut
// off -- so a send-only link HELLOs once a second forever, never comes up, and
// every Send it is handed is refused for want of a session. What it produces is
// a steady trickle of frames no peer can answer and a mod whose author is told
// nothing. So the gate is not a tuning knob and it is not a partial mode
// either: below the floor Open says so, once, and nothing else happens.
//
// IT GATES ON THE ENGINE, NOT ON THE API PIN, and the two are separate axes.
// The pin is a build-time fact -- which runtime-api.json the bindings came
// from -- and every member this package touches (send_udp, recv_udp,
// write_file, game_version, on_udp_packet_received) exists in the 2.0.77
// description, which shipped with 2.0.59. The engine is what
// helpers.game_version reports at RUN TIME. So a mod built at the
// general-availability pin gets the whole library on a 2.1.14 engine with no
// rebuild, no repin and no second build of the guest.

// Version is a Factorio version triple.
type Version struct{ Major, Minor, Patch uint16 }

// MinEngineVersion is the lowest base-game version this library will run on at
// all. Below it Open refuses, Pump does nothing, and every API call answers
// StatusDisabled.
//
// IT WAS CALLED BaseFloorRecv while the floor gated only the receive path, and
// the rename is the whole of what the hard-disable changed about its meaning.
// "Recv" named a mechanism; this names the axis -- the ENGINE, which is not the
// API pin -- and the scope, which is now the library rather than one call.
//
// THE VALUE IS WHAT WAS MEASURED, NOT WHERE THE FIX LANDED. The crash is
// confirmed present at 2.0.77 here, and was reported upstream at 2.1.9; inbound
// is confirmed WORKING at 2.1.14 here -- the arm that kills 2.0.77 survived 25 s
// and delivered 467 events, and a full handshake ran over 61. The versions
// between are unverified, so the floor is the version that was actually
// observed to work rather than the version somebody's changelog says was fixed.
// Lowering it wants a probe run at the version being lowered to, not an
// argument.
//
// scripts/lib-engine.sh READS THIS DECLARATION with a sed, so that the in-game
// gates can refuse early on an engine that cannot possibly pass rather than
// timing out leg by leg. Respelling the line is allowed; the reader fails
// loudly rather than defaulting when it can no longer parse it.
var MinEngineVersion = Version{2, 1, 14}

// Less is a plain lexicographic compare over the triple.
func (v Version) Less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

func (v Version) String() string {
	return itoa(uint32(v.Major)) + "." + itoa(uint32(v.Minor)) + "." + itoa(uint32(v.Patch))
}

// Zero reports a version nothing has filled in, which the gate treats as
// "below the floor". A failed read must never open the link.
func (v Version) Zero() bool { return v == Version{} }

// ParseVersion reads "2.1.14", and anything trailing it.
//
// Hand-rolled rather than strings.Split + strconv: both pull real weight into a
// guest, and the scaffold's own comment already names the strconv-and-plus
// shape as the trap a downstream mod measured as its entire guest heap. This
// allocates nothing.
func ParseVersion(s string) (Version, bool) {
	var v Version
	i := 0
	for f := 0; f < 3; f++ {
		start := i
		var n uint32
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			n = n*10 + uint32(s[i]-'0')
			if n > 65535 {
				return Version{}, false
			}
			i++
		}
		if i == start {
			return Version{}, false
		}
		switch f {
		case 0:
			v.Major = uint16(n)
		case 1:
			v.Minor = uint16(n)
		case 2:
			v.Patch = uint16(n)
		}
		if f < 2 {
			if i >= len(s) || s[i] != '.' {
				return Version{}, false
			}
			i++
		}
	}
	return v, true
}

// disabledMessage is the one line the gate logs, built without fmt.
//
// IT IS A LOG LINE AND NOT A COUNTER, deliberately, and it is the one thing
// this package does that is per-peer on purpose. Whether an engine is below the
// floor is identical on every peer -- Factorio refuses a multiplayer connection
// between two different builds -- so this COULD be guest state. What must not
// be guest state is anything about whether the log call itself worked, and the
// game log is not CRC'd, which is exactly why fk.Log is this repo's only
// sanctioned sink for a per-peer fact. See the join-safety contract in doc.go.
func disabledMessage(have Version) string {
	s := "fkipc: disabled -- requires Factorio >= " + MinEngineVersion.String() +
		"; this engine is "
	if have.Zero() {
		// A failed read is treated as below the floor, and saying "0.0.0" would
		// invite somebody to go looking for a 0.0.0 Factorio.
		return s + "unreadable"
	}
	return s + have.String()
}
