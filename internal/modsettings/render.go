package modsettings

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// RenderJSON writes a decoded document back out in the shape ParseJSON reads.
//
// WHY THE INVERSE EXISTS AT ALL. The engine REWRITES mod-settings.dat after a
// run with the values it settled on (measured), and "it settled on" is where
// the interesting behaviour is: a number outside the range and a dropdown value
// outside the allowed list are reset to the default rather than kept. A gate
// that only writes the file has to take on faith that the engine read it; a
// gate that reads the rewrite back can assert it, and this is the half that
// makes the assertion readable.
//
// THE TYPE OF A NUMBER SURVIVES, which is the one thing a naive renderer would
// lose. ParseJSON types a number by its spelling, so this spells a signed
// 64-bit as an integer literal and a double with a fractional part or an
// exponent, and never the other way round: rendering 20.0 as `20` would turn a
// double setting into an int setting the next time the file was written.
//
// KEY ORDER IS THE FILE'S ORDER, for the same reason ParseJSON keeps it: this
// output is parsed and re-encoded by tests and by anything driving the command,
// and a rendering that sorted would break the byte identity that makes those
// worth running.
//
// EVERYTHING IT CANNOT SAY IS A REFUSAL NAMING THE KEY rather than a silent
// normalisation. The JSON form describes exactly the file the engine writes: a
// three-section root, each setting a dictionary holding one `value`, every
// any-type byte clear and the header flag zero. A file outside that shape is
// still readable with Decode, and turning it into JSON that parsed back into a
// DIFFERENT file is the failure this refuses. BYTES THAT ARE NOT UTF-8 ARE THAT
// FAILURE and they are the one that looks like nothing: encoding/json replaces
// each invalid byte with U+FFFD rather than refusing, so a file holding one
// rendered, parsed back and re-encoded to a DIFFERENT file with nothing said
// (measured: 114 bytes in, 116 out). A setting name and a stored setting are
// both checked before either is written.
func RenderJSON(doc Document) ([]byte, error) {
	if doc.Flag != 0 {
		return nil, errors.New("modsettings: the header flag is " + strconv.Itoa(int(doc.Flag)) +
			" and the JSON form has no way to say that; the engine writes 0")
	}
	if doc.Root.Kind != KindDict {
		return nil, errors.New("modsettings: the root node is " + kindName(doc.Root.Kind) +
			" and the JSON form describes a dictionary of the three stages")
	}
	if doc.Root.AnyType {
		return nil, errors.New("modsettings: the root node has its any-type byte set and the JSON form has no way to say that")
	}

	seen := map[string]bool{}
	rendered := map[string]string{}
	for _, section := range doc.Root.Dict {
		if !contains(stageOrder, section.Key) {
			return nil, errors.New("modsettings: the root names " + strconv.Quote(section.Key) +
				", which is not one of " + strings.Join(stageOrder, ", "))
		}
		if seen[section.Key] {
			return nil, errors.New("modsettings: the root names the section " + section.Key + " twice")
		}
		seen[section.Key] = true
		body, err := renderSection(section.Key, section.Value)
		if err != nil {
			return nil, err
		}
		rendered[section.Key] = body
	}

	var out bytes.Buffer
	out.WriteString("{\n  \"version\": [")
	for i, v := range doc.Version {
		if i > 0 {
			out.WriteString(", ")
		}
		out.WriteString(strconv.Itoa(int(v)))
	}
	out.WriteString("]")
	for _, name := range stageOrder {
		body, ok := rendered[name]
		if !ok {
			// A section the file does not carry is left out rather than
			// invented: ParseJSON puts an empty one back, so the round trip
			// still closes, and inventing one here would report a file that
			// does not exist.
			continue
		}
		out.WriteString(",\n  " + quote(name) + ": " + body)
	}
	out.WriteString("\n}\n")
	return out.Bytes(), nil
}

// renderSection renders one stage's settings, in file order, as an object at
// one level of indentation.
func renderSection(name string, v Value) (string, error) {
	if v.Kind != KindDict {
		return "", errors.New("modsettings: the section " + name + " is " + kindName(v.Kind) +
			" and every section of a mod-settings.dat is a dictionary")
	}
	if v.AnyType {
		return "", errors.New("modsettings: the section " + name +
			" has its any-type byte set and the JSON form has no way to say that")
	}
	if len(v.Dict) == 0 {
		return "{}", nil
	}
	var out bytes.Buffer
	out.WriteString("{\n")
	for i, e := range v.Dict {
		if !utf8.ValidString(e.Key) {
			return "", errors.New("modsettings: the section " + name + " names the setting " +
				strconv.Quote(e.Key) + ", which holds bytes that are not UTF-8, and JSON has no way to say that")
		}
		where := name + "." + e.Key
		text, err := renderSetting(where, e.Value)
		if err != nil {
			return "", err
		}
		out.WriteString("    " + quote(e.Key) + ": " + text)
		if i != len(v.Dict)-1 {
			out.WriteString(",")
		}
		out.WriteString("\n")
	}
	out.WriteString("  }")
	return out.String(), nil
}

// renderSetting unwraps the one-key dictionary the engine stores a setting as
// and renders the scalar inside it.
func renderSetting(where string, v Value) (string, error) {
	if v.Kind != KindDict {
		return "", errors.New("modsettings: " + where + " is " + kindName(v.Kind) +
			" and a stored setting is a dictionary holding one key, value")
	}
	if v.AnyType {
		return "", errors.New("modsettings: " + where +
			" has its any-type byte set and the JSON form has no way to say that")
	}
	if len(v.Dict) != 1 || v.Dict[0].Key != "value" {
		var keys []string
		for _, e := range v.Dict {
			keys = append(keys, e.Key)
		}
		return "", errors.New("modsettings: " + where + " holds " + strconv.Itoa(len(v.Dict)) +
			" keys (" + strings.Join(keys, ", ") + ") and a stored setting holds exactly one, value")
	}
	return renderScalar(where, v.Dict[0].Value)
}

func renderScalar(where string, v Value) (string, error) {
	if v.AnyType {
		return "", errors.New("modsettings: the value of " + where +
			" has its any-type byte set and the JSON form has no way to say that")
	}
	switch v.Kind {
	case KindBool:
		if v.Bool {
			return "true", nil
		}
		return "false", nil
	case KindInt:
		return strconv.FormatInt(v.Int, 10), nil
	case KindNumber:
		return renderDouble(where, v.Number)
	case KindString:
		if !utf8.ValidString(v.Str) {
			return "", errors.New("modsettings: the value of " + where +
				" holds bytes that are not UTF-8, and JSON has no way to say that")
		}
		return quote(v.Str), nil
	}
	return "", errors.New("modsettings: the value of " + where + " is " + kindName(v.Kind) +
		" and a setting holds a bool, a number or a string")
}

// renderDouble writes the shortest text that reads back to exactly this double,
// and makes sure it still LOOKS like a double.
//
// strconv's shortest form drops a fraction that is zero, so 20.0 comes out as
// "20" and ParseJSON would then read it as the signed 64-bit type. A trailing
// ".0" is what carries the type across, and it is only ever appended when the
// shortest form has neither a point nor an exponent, so no digit of the value
// is touched.
func renderDouble(where string, f float64) (string, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", errors.New("modsettings: the value of " + where + " is " +
			strconv.FormatFloat(f, 'g', -1, 64) + ", which JSON cannot write")
	}
	text := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(text, ".eE") {
		text += ".0"
	}
	return text, nil
}

// quote writes a JSON string with the escaping encoding/json uses, minus the
// HTML escaping: a setting is an ingredient list or a mod name, not a fragment
// of a web page, and < in the middle of one is unreadable for nothing.
func quote(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		// encoding/json cannot fail on a string: invalid UTF-8 is REPLACED
		// rather than refused, which is exactly why every caller checks
		// utf8.ValidString before getting here. Kept as a total function so
		// callers have one less error to plumb, with the fallback spelled
		// rather than assumed.
		return strconv.Quote(s)
	}
	return strings.TrimRight(buf.String(), "\n")
}

func kindName(k Kind) string {
	switch k {
	case KindBool:
		return "a bool"
	case KindNumber:
		return "a double"
	case KindString:
		return "a string"
	case KindDict:
		return "a dictionary"
	case KindInt:
		return "a signed 64-bit number"
	}
	return "node type " + strconv.Itoa(int(k))
}
