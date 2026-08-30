// Command logdump is the end-to-end fixture for the LINE BUILDER and the VALUE
// DUMPER, which are the two halves of one story: a guest that wants to say what
// it is doing without paying for it forever.
//
// fklog builds a line in one fixed buffer and hands it to the host borrowing
// that buffer, so nothing is allocated; Value.Dump writes into a destination the
// caller owns, and fklog's own tail is where a caller most often wants it. The
// two are separate packages on purpose -- fklog depends on fk alone, because
// fkapi is generated and pinned and a line builder has no business dragging a
// pin into a consumer that only wanted one.
//
// EVERY LINE HERE IS ORDER-INDEPENDENT, deliberately: the tier-2 value is built
// by the GUEST rather than read back from a host table, so its pair order is the
// guest's own and the dump is a function of the program.
package main

import (
	"github.com/Techrocket9/fklua/guest/go/fk"
	"github.com/Techrocket9/fklua/guest/go/fkapi"
	"github.com/Techrocket9/fklua/guest/go/fklog"
)

//go:wasmexport fk_on_init
func onInit() {
	// THE APPENDERS, one line, no allocation.
	fklog.Start("nums ")
	fklog.U(0)
	fklog.S(" ")
	fklog.U(42)
	fklog.S(" ")
	fklog.I(-7)
	fklog.S(" ")
	fklog.B(true)
	fklog.S(" ")
	fklog.B(false)
	fklog.End()

	// THE SIGNED EDGE, which is the divergence one hand-written copy grew: -v
	// overflows at the most negative value and prints it as itself.
	fklog.Start("edge ")
	fklog.I(-9223372036854775808)
	fklog.S(" ")
	fklog.U(18446744073709551615)
	fklog.End()

	// ONE DECIMAL, INCLUDING THE CARRY. 9.96 rounds to 10.0 and not to 9.10.
	fklog.Start("f1 ")
	fklog.F1(1.25)
	fklog.S(" ")
	fklog.F1(9.96)
	fklog.S(" ")
	fklog.F1(-0.04)
	fklog.End()

	// TRUNCATION OVER GROWTH. A line longer than the buffer is cut, and the cut
	// is at the buffer's size rather than anywhere else.
	fklog.Start("")
	for i := 0; i < 200; i++ {
		fklog.S("0123456789")
	}
	fk.Log("trunc " + itoa(fklog.Len()))

	// THE DUMPER, through fklog's own tail, which is the seam that keeps the
	// two packages apart.
	v := fkapi.OfMap(
		fkapi.KeyValue{Key: fkapi.OfString("name"), Val: fkapi.OfString("belt")},
		fkapi.KeyValue{Key: fkapi.OfString("count"), Val: fkapi.OfNumber(42)},
		fkapi.KeyValue{Key: fkapi.OfString("ratio"), Val: fkapi.OfNumber(1.5)},
		fkapi.KeyValue{Key: fkapi.OfString("on"), Val: fkapi.OfBool(true)},
		fkapi.KeyValue{Key: fkapi.OfString("gone"), Val: fkapi.OfNil()},
		fkapi.KeyValue{Key: fkapi.OfString("list"), Val: fkapi.OfArray(
			fkapi.OfNumber(1), fkapi.OfString("two"), fkapi.OfBool(false))},
		fkapi.KeyValue{Key: fkapi.OfString("inner"), Val: fkapi.OfMap(
			fkapi.KeyValue{Key: fkapi.OfString("deep"), Val: fkapi.OfNumber(7)})},
		fkapi.KeyValue{Key: fkapi.OfNumber(7), Val: fkapi.OfString("seven")},
	)
	fklog.Start("dump ")
	fklog.Advance(v.Dump(fklog.Tail()))
	fklog.End()

	// A SCALAR AT THE TOP LEVEL, and the empty containers, which is where a
	// recursive renderer most often gets a separator wrong.
	fklog.Start("scalars ")
	fklog.Advance(fkapi.OfNumber(-0.5).Dump(fklog.Tail()))
	fklog.S(" ")
	fklog.Advance(fkapi.OfNil().Dump(fklog.Tail()))
	fklog.S(" ")
	fklog.Advance(fkapi.OfArray().Dump(fklog.Tail()))
	fklog.S(" ")
	fklog.Advance(fkapi.OfMap().Dump(fklog.Tail()))
	fklog.End()

	// THE DUMPER TRUNCATES TOO, and reports what fitted rather than what it
	// wanted. Eight bytes of room for a value that needs more.
	var small [8]byte
	fk.Log("dumptrunc " + itoa(fkapi.OfString("0123456789").Dump(small[:])) +
		" " + string(small[:]))
}

// itoa is the one thing this fixture does NOT use fklog for, because the
// truncation line has to report fklog's own length while its buffer is full.
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

func main() {}
