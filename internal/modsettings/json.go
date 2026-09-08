package modsettings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The three stages a mod-settings.dat carries, in the order the engine writes
// them. All three are always present in the file, empty or not: a reader that
// meets a file missing one is reading a file this writer did not produce.
var stageOrder = []string{"startup", "runtime-global", "runtime-per-user"}

// ParseJSON reads the JSON description of a settings file and builds the
// document Encode writes.
//
// THE SHAPE:
//
//	{"comment": "why this row exists",
//	 "version": [2, 0, 77, 0],
//	 "startup": {"my-mod-a-setting": 3, "my-mod-another": false},
//	 "runtime-global": {},
//	 "runtime-per-user": {}}
//
// A section this object does not name comes out as an empty dictionary, which
// is what the engine's own file carries for a stage nothing has set.
//
// A "comment" KEY IS READ AND IGNORED, and it is here because JSON has no
// comment syntax and a committed settings row has to be able to say what it is
// for. It holds a string and reaches no byte of the dat.
//
// THE TYPE OF A NUMBER IS ITS SPELLING, and that is a decision rather than an
// accident: 40 is written as the signed 64-bit type and 40.0 as the double,
// because an int setting and a double setting are different prototypes and the
// file should say which one a row is for. The engine reads either encoding as
// the same number (measured), so this is about the file being honest rather
// than about the engine needing it.
//
// KEY ORDER IS THE FILE'S ORDER. encoding/json's map decoding loses it and Go's
// map iteration is randomised on purpose, so this walks the token stream
// instead: what the JSON says first is what the dat holds first, which is what
// makes a byte comparison against a written file mean anything.
func ParseJSON(data []byte) (Document, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	if err := expectDelim(dec, '{', "the document"); err != nil {
		return Document{}, err
	}

	doc := Document{Version: [4]uint16{2, 0, 0, 0}}
	sections := map[string]Value{}
	seenVersion := false
	seenComment := false

	for dec.More() {
		key, err := readKey(dec, "the document")
		if err != nil {
			return Document{}, err
		}
		switch {
		case key == "comment":
			// A KEY NAMED TWICE IS REFUSED WHEREVER IT SITS. A duplicate
			// section and a duplicate setting are refused because the later one
			// would silently win, and that is just as true one level up: a
			// document carrying two versions is a document somebody edited
			// wrong, and picking one of them is picking which header the engine
			// reads without saying so.
			if seenComment {
				return Document{}, errors.New("modsettings: the JSON names comment twice")
			}
			seenComment = true
			tok, err := dec.Token()
			if err != nil {
				return Document{}, errors.New("modsettings: reading the comment: " + err.Error())
			}
			if _, ok := tok.(string); !ok {
				return Document{}, fmt.Errorf("modsettings: the comment holds %v, and a comment is a string", tok)
			}
		case key == "version":
			if seenVersion {
				return Document{}, errors.New("modsettings: the JSON names version twice")
			}
			v, err := readVersion(dec)
			if err != nil {
				return Document{}, err
			}
			doc.Version = v
			seenVersion = true
		case contains(stageOrder, key):
			if _, dup := sections[key]; dup {
				return Document{}, errors.New("modsettings: the JSON names the section " + key + " twice")
			}
			section, err := readSection(dec, key)
			if err != nil {
				return Document{}, err
			}
			sections[key] = section
		default:
			return Document{}, errors.New("modsettings: the JSON names " + strconv.Quote(key) +
				", which is neither comment, nor version, nor one of " + strings.Join(stageOrder, ", "))
		}
	}
	if err := expectDelim(dec, '}', "the document"); err != nil {
		return Document{}, err
	}
	if err := atEOF(dec); err != nil {
		return Document{}, err
	}
	if !seenVersion {
		return Document{}, errors.New("modsettings: the JSON names no version; the engine reads the header before anything else")
	}

	root := Value{Kind: KindDict}
	for _, name := range stageOrder {
		section, ok := sections[name]
		if !ok {
			section = Value{Kind: KindDict}
		}
		root.Dict = append(root.Dict, Entry{Key: name, Value: section})
	}
	doc.Root = root
	return doc, nil
}

// readSection reads one stage's object of setting name to value, and wraps each
// value in the one-key dictionary the engine stores a setting as.
func readSection(dec *json.Decoder, name string) (Value, error) {
	if err := expectDelim(dec, '{', name); err != nil {
		return Value{}, err
	}
	section := Value{Kind: KindDict}
	seen := map[string]bool{}
	for dec.More() {
		key, err := readKey(dec, name)
		if err != nil {
			return Value{}, err
		}
		if seen[key] {
			return Value{}, errors.New("modsettings: " + name + " names the setting " + key +
				" twice; the later one would silently win")
		}
		seen[key] = true
		tok, err := dec.Token()
		if err != nil {
			return Value{}, errors.New("modsettings: reading the value of " + key + ": " + err.Error())
		}
		v, err := scalar(tok, key)
		if err != nil {
			return Value{}, err
		}
		section.Dict = append(section.Dict,
			Entry{Key: key, Value: Dict(Pair("value", v))})
	}
	if err := expectDelim(dec, '}', name); err != nil {
		return Value{}, err
	}
	return section, nil
}

// scalar turns one JSON token into a node. A composite value is refused rather
// than flattened: a setting holds a bool, a number or a string and nothing
// else, so an array here is a mistake in the file and not a shape to guess at.
func scalar(tok json.Token, key string) (Value, error) {
	switch t := tok.(type) {
	case bool:
		return Bool(t), nil
	case string:
		return Str(t), nil
	case json.Number:
		text := t.String()
		if strings.ContainsAny(text, ".eE") {
			f, err := t.Float64()
			if err != nil {
				return Value{}, errors.New("modsettings: " + key + " holds " + text +
					", which is not a number this writer can hold: " + err.Error())
			}
			return Number(f), nil
		}
		i, err := t.Int64()
		if err != nil {
			return Value{}, errors.New("modsettings: " + key + " holds " + text +
				", which does not fit a signed 64-bit number: " + err.Error())
		}
		return Int(i), nil
	case json.Delim:
		return Value{}, errors.New("modsettings: " + key +
			" holds " + delimName(t) + "; a setting holds a bool, a number or a string")
	case nil:
		return Value{}, errors.New("modsettings: " + key +
			" holds null; a setting holds a bool, a number or a string")
	}
	return Value{}, fmt.Errorf("modsettings: %s holds %v, which is not a value this writer knows", key, tok)
}

func readVersion(dec *json.Decoder) ([4]uint16, error) {
	var out [4]uint16
	if err := expectDelim(dec, '[', "version"); err != nil {
		return out, err
	}
	n := 0
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return out, errors.New("modsettings: reading version: " + err.Error())
		}
		num, ok := tok.(json.Number)
		if !ok {
			return out, fmt.Errorf("modsettings: version holds %v, and every part of a version is a number", tok)
		}
		i, err := num.Int64()
		if err != nil || i < 0 || i > 65535 {
			return out, errors.New("modsettings: version holds " + num.String() +
				", and every part of a version is a whole number from 0 to 65535")
		}
		if n == len(out) {
			return out, errors.New("modsettings: version has more than " +
				strconv.Itoa(len(out)) + " parts")
		}
		out[n] = uint16(i)
		n++
	}
	if err := expectDelim(dec, ']', "version"); err != nil {
		return out, err
	}
	if n != len(out) {
		return out, errors.New("modsettings: version has " + strconv.Itoa(n) +
			" parts and the header takes exactly " + strconv.Itoa(len(out)))
	}
	return out, nil
}

func readKey(dec *json.Decoder, where string) (string, error) {
	tok, err := dec.Token()
	if err != nil {
		return "", errors.New("modsettings: reading a key of " + where + ": " + err.Error())
	}
	key, ok := tok.(string)
	if !ok {
		return "", fmt.Errorf("modsettings: %s has the key %v, which is not a name", where, tok)
	}
	return key, nil
}

func expectDelim(dec *json.Decoder, want rune, where string) error {
	tok, err := dec.Token()
	if err != nil {
		return errors.New("modsettings: reading " + where + ": " + err.Error())
	}
	d, ok := tok.(json.Delim)
	if !ok || rune(d) != want {
		return fmt.Errorf("modsettings: %s begins with %v, not %c", where, tok, want)
	}
	return nil
}

// atEOF refuses a second document after the first. json.Decoder happily reads a
// stream of them and a file with two objects in it is a file somebody edited
// wrong, not a file with a spare.
func atEOF(dec *json.Decoder) error {
	if dec.More() {
		return errors.New("modsettings: the JSON carries more than one document")
	}
	return nil
}

// delimName names a composite with its article attached, because the article is
// part of the word here: "a list" and "an object" cannot share one prefix.
func delimName(d json.Delim) string {
	if d == '[' || d == ']' {
		return "a list"
	}
	return "an object"
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
