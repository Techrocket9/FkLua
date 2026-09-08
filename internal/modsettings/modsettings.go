// Package modsettings reads and writes Factorio's mod-settings.dat.
//
// WHY A COMPILER SHIPS A BINARY FORMAT READER. The in-game data-stage gate runs
// the settings and data stages under `factorio --dump-data`, and until this
// existed it could only ever run them on the values a mod DECLARES. A player's
// chosen value reaches the stages through a file the engine writes and reads
// back, and nothing else writes it: there is no CLI, no text form and no flag.
// So a headless gate that wants to see the other side of a setting has to write
// those bytes itself, and every library on the channel gets that gate by
// writing its own JSON rather than its own encoder.
//
// THE LAYOUT IS MEASURED, not read off a wiki page. It was measured downstream,
// in FkRecipes (same author, MIT), against Factorio 2.0.77 (build 84539,
// mac-arm64, steam) in two directions: the install's own 196-byte
// mod-settings.dat was decoded by a reader that consumed exactly 196 of 196
// bytes, and the engine was then handed an encoded file and read it. What that
// measurement says:
//
//	version   four u16 little-endian, then one u8
//	node      a type byte, then an any-type byte, then the payload
//	1 bool    one byte
//	2 double  eight bytes, little-endian IEEE 754
//	3 string  an empty flag byte; when it is 0, a space-optimised length
//	          (one u8, or 255 followed by a u32 little-endian) and then the
//	          UTF-8 bytes
//	5 dict    a u32 little-endian count, then that many (string key, node)
//	          pairs, the key written as a string payload with no node header
//	6 int     eight bytes, little-endian, signed
//
// The root is a dictionary carrying startup, runtime-global and
// runtime-per-user, each a dictionary of setting name to a dictionary holding
// one key, value.
//
// FOUR MORE ENGINE BEHAVIOURS, measured the same way and load-bearing for
// anything that writes this file: the engine reads MODS/mod-settings.dat beside
// the mod folders rather than inside one, and REWRITES it after the run with
// the values it settled on; a stored number outside a setting's range and a
// stored dropdown value outside its allowed values are RESET to the default
// rather than clamped or refused; a stored string is not trimmed, whatever
// auto_trim says, because auto_trim is a GUI behaviour; and a value stored
// under a name no mod declares is ignored and survives the rewrite.
//
// EVERYTHING HERE IS ORDERED. A dictionary is a slice of pairs and never a Go
// map, because what this writes is compared byte for byte against a committed
// golden and Go's map iteration is deliberately random.
package modsettings

import (
	"encoding/binary"
	"errors"
	"math"
	"strconv"
)

// Kind is a property-tree node type, with the engine's own numbering.
type Kind uint8

// The node types this format uses. 0 (none) and 4 (list) exist in the engine's
// property tree and never appear in a mod-settings.dat, so they are not
// implemented: Decode names an unknown type rather than guessing at it.
const (
	KindBool   Kind = 1
	KindNumber Kind = 2
	KindString Kind = 3
	KindDict   Kind = 5
	KindInt    Kind = 6
)

// Value is one property-tree node.
//
// THE ANY-TYPE BYTE IS CARRIED RATHER THAN ASSUMED. It sits on every node in
// the file and the engine writes 0 for all of them, but a reader that dropped
// it could not round trip a file the engine wrote differently, and a round trip
// that quietly normalises is a round trip that proves nothing.
type Value struct {
	Kind    Kind
	AnyType bool

	Bool   bool    // KindBool
	Number float64 // KindNumber
	Str    string  // KindString
	Int    int64   // KindInt
	Dict   []Entry // KindDict, in file order
}

// Entry is one key and node of a dictionary, in the order it appears.
type Entry struct {
	Key   string
	Value Value
}

// Document is a whole file: the version header and the root node.
type Document struct {
	// Version is the four u16 the header opens with, in file order.
	Version [4]uint16
	// Flag is the u8 that closes the header. The engine writes 0.
	Flag byte
	Root Value
}

// Bool, Number, Str and Int build the leaves; Dict builds a node from pairs.
func Bool(b bool) Value      { return Value{Kind: KindBool, Bool: b} }
func Number(f float64) Value { return Value{Kind: KindNumber, Number: f} }
func Str(s string) Value     { return Value{Kind: KindString, Str: s} }
func Int(i int64) Value      { return Value{Kind: KindInt, Int: i} }

// Dict builds a dictionary node from pairs, in the order given.
func Dict(entries ...Entry) Value {
	return Value{Kind: KindDict, Dict: entries}
}

// Pair is one dictionary entry.
func Pair(key string, v Value) Entry { return Entry{Key: key, Value: v} }

// Encode writes a document. It cannot fail: every Value this package builds is
// writable, and a Value with a Kind this package does not know is written as
// nothing at all rather than as a wrong node, which Decode then names.
func Encode(doc Document) []byte {
	buf := make([]byte, 0, 256)
	var head [2]byte
	for _, v := range doc.Version {
		binary.LittleEndian.PutUint16(head[:], v)
		buf = append(buf, head[0], head[1])
	}
	buf = append(buf, doc.Flag)
	return appendValue(buf, doc.Root)
}

func appendValue(buf []byte, v Value) []byte {
	buf = append(buf, byte(v.Kind))
	buf = append(buf, boolByte(v.AnyType))
	switch v.Kind {
	case KindBool:
		buf = append(buf, boolByte(v.Bool))
	case KindNumber:
		buf = appendU64(buf, math.Float64bits(v.Number))
	case KindString:
		buf = appendString(buf, v.Str)
	case KindInt:
		buf = appendU64(buf, uint64(v.Int))
	case KindDict:
		var n [4]byte
		binary.LittleEndian.PutUint32(n[:], uint32(len(v.Dict)))
		buf = append(buf, n[:]...)
		for _, e := range v.Dict {
			buf = appendString(buf, e.Key)
			buf = appendValue(buf, e.Value)
		}
	}
	return buf
}

// appendString writes the empty flag and, when there is anything to write, the
// space-optimised length and the bytes.
//
// THE EMPTY FLAG IS NOT A LENGTH OF ZERO. An empty string is one byte and
// nothing else: writing a zero length after the flag would push every later
// byte along by one and the engine would read the file as corrupt.
func appendString(buf []byte, s string) []byte {
	if s == "" {
		return append(buf, 1)
	}
	buf = append(buf, 0)
	// The space optimisation: one byte up to 254, and 255 as the escape into a
	// u32. 255 itself therefore takes the long form.
	if len(s) < 255 {
		buf = append(buf, byte(len(s)))
	} else {
		var n [4]byte
		binary.LittleEndian.PutUint32(n[:], uint32(len(s)))
		buf = append(buf, 255)
		buf = append(buf, n[:]...)
	}
	return append(buf, s...)
}

func appendU64(buf []byte, u uint64) []byte {
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], u)
	return append(buf, n[:]...)
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

// Decode reads a whole file and REFUSES anything it does not understand: a
// short read, an unknown node type, a length that runs off the end, and bytes
// left over at the end. Every refusal names the byte it stopped at.
//
// THE TRAILING-BYTES CHECK IS THE ONE THAT CAUGHT THE FORMAT. The probe that
// established this layout is "the reader consumed exactly 196 of 196 bytes"; a
// reader that stops early is a reader that has misread a length somewhere
// upstream and would otherwise report success on a file it does not
// understand.
func Decode(b []byte) (Document, error) {
	d := &reader{buf: b}
	var doc Document
	for i := range doc.Version {
		v, err := d.u16()
		if err != nil {
			return Document{}, err
		}
		doc.Version[i] = v
	}
	flag, err := d.byteAt()
	if err != nil {
		return Document{}, err
	}
	doc.Flag = flag
	root, err := d.value(0)
	if err != nil {
		return Document{}, err
	}
	doc.Root = root
	if d.at != len(d.buf) {
		return Document{}, errors.New("modsettings: " + bytesAre(len(d.buf)-d.at) +
			" left over after the root node, at offset " + strconv.Itoa(d.at) +
			"; the file is not the shape this reader knows")
	}
	return doc, nil
}

// bytesAre spells a byte count with the verb that agrees with it. One spare
// byte is the commonest way this refusal fires -- a file with a byte glued to
// the end of it -- and "1 bytes are left over" reads as a bug in the reader
// reporting the bug.
func bytesAre(n int) string {
	if n == 1 {
		return "1 byte is"
	}
	return strconv.Itoa(n) + " bytes are"
}

// maxDepth stops a hand-edited or corrupt file from turning a cycle of
// dictionaries into a stack overflow. A real file is three deep.
const maxDepth = 32

type reader struct {
	buf []byte
	at  int
}

func (r *reader) short(what string, n int) error {
	return errors.New("modsettings: the file ends in the middle of " + what +
		"; " + strconv.Itoa(n) + " bytes were wanted at offset " + strconv.Itoa(r.at) +
		" and " + strconv.Itoa(len(r.buf)-r.at) + " are left")
}

func (r *reader) take(what string, n int) ([]byte, error) {
	if n < 0 || len(r.buf)-r.at < n {
		return nil, r.short(what, n)
	}
	out := r.buf[r.at : r.at+n]
	r.at += n
	return out, nil
}

func (r *reader) byteAt() (byte, error) {
	b, err := r.take("a byte", 1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (r *reader) u16() (uint16, error) {
	b, err := r.take("a 16-bit number", 2)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(b), nil
}

func (r *reader) u32() (uint32, error) {
	b, err := r.take("a 32-bit number", 4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

func (r *reader) u64() (uint64, error) {
	b, err := r.take("a 64-bit number", 8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b), nil
}

func (r *reader) str() (string, error) {
	empty, err := r.byteAt()
	if err != nil {
		return "", err
	}
	if empty != 0 {
		return "", nil
	}
	n, err := r.byteAt()
	if err != nil {
		return "", err
	}
	length := int(n)
	if n == 255 {
		long, err := r.u32()
		if err != nil {
			return "", err
		}
		// take is what bounds this, and it is the only thing that can: a u32
		// widened to a 64-bit int is never negative, so a test against zero
		// here would be a check that cannot fire. take refuses both halves of
		// what a bad length means -- a negative one, which is what the same
		// conversion produces where int is 32 bits, and one that runs off the
		// end of the file, which is what a 4 GiB claim in a 105-byte file is.
		length = int(long)
	}
	b, err := r.take("a string of "+strconv.Itoa(length)+" bytes", length)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (r *reader) value(depth int) (Value, error) {
	if depth > maxDepth {
		return Value{}, errors.New("modsettings: the property tree at offset " +
			strconv.Itoa(r.at) + " nests deeper than " + strconv.Itoa(maxDepth) +
			" levels; a mod-settings.dat is three deep")
	}
	kind, err := r.byteAt()
	if err != nil {
		return Value{}, err
	}
	anyType, err := r.byteAt()
	if err != nil {
		return Value{}, err
	}
	v := Value{Kind: Kind(kind), AnyType: anyType != 0}
	switch v.Kind {
	case KindBool:
		b, err := r.byteAt()
		if err != nil {
			return Value{}, err
		}
		v.Bool = b != 0
	case KindNumber:
		u, err := r.u64()
		if err != nil {
			return Value{}, err
		}
		v.Number = math.Float64frombits(u)
	case KindString:
		s, err := r.str()
		if err != nil {
			return Value{}, err
		}
		v.Str = s
	case KindInt:
		u, err := r.u64()
		if err != nil {
			return Value{}, err
		}
		v.Int = int64(u)
	case KindDict:
		n, err := r.u32()
		if err != nil {
			return Value{}, err
		}
		// The count is read before the entries and a corrupt one could claim
		// four billion, so nothing is preallocated from it: each entry is
		// appended as it is actually read and a short file runs out first.
		for i := uint32(0); i < n; i++ {
			key, err := r.str()
			if err != nil {
				return Value{}, err
			}
			child, err := r.value(depth + 1)
			if err != nil {
				return Value{}, err
			}
			v.Dict = append(v.Dict, Entry{Key: key, Value: child})
		}
	default:
		return Value{}, errors.New("modsettings: node type " + strconv.Itoa(int(kind)) +
			" at offset " + strconv.Itoa(r.at-2) +
			" is not one this reader knows (1 bool, 2 double, 3 string, 5 dictionary, 6 signed 64-bit)")
	}
	return v, nil
}
