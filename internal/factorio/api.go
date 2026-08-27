package factorio

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// This file models runtime-api.json, the machine-readable description of
// Factorio's Lua API that ships with the game and is also published per version
// at https://lua-api.factorio.com/<version>/runtime-api.json.
//
// It is the input to every binding FkLua generates, so the parser's job is to
// lose nothing. A field silently dropped here becomes a member the generator
// never emits, and the symptom is a mod author finding that one method out of
// 3774 does not exist -- with nothing in the build to say why. TestAPICensus
// pins the shape counts for exactly that reason.
//
// TWO VERSION AXES, and confusing them is a real mistake:
//
//	APIVersion         the SCHEMA version. 6, and stable across game versions --
//	                   every description committed under api/ reports 6, so one
//	                   generator serves every data set we support.
//	ApplicationVersion the GAME version, which is what actually changes.

// API is one parsed runtime-api.json.
type API struct {
	APIVersion int `json:"api_version"`
	// Application is always "factorio". It exists because the same schema
	// describes the prototype stage and, in principle, other tools' dumps.
	Application        string         `json:"application"`
	ApplicationVersion string         `json:"application_version"`
	Stage              string         `json:"stage"`
	Classes            []Class        `json:"classes"`
	Events             []Event        `json:"events"`
	Concepts           []Concept      `json:"concepts"`
	Defines            []Define       `json:"defines"`
	GlobalObjects      []GlobalObject `json:"global_objects"`
	GlobalFunctions    []Method       `json:"global_functions"`
}

// Class is a LuaObject type: LuaEntity, LuaSurface, and 154 others.
type Class struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Abstract    bool        `json:"abstract"`
	Order       int         `json:"order"`
	Methods     []Method    `json:"methods"`
	Attributes  []Attribute `json:"attributes"`
	// Parent is the class this one inherits from, empty when it has none.
	// Inheritance is real here: a member reached through a parent is callable on
	// the child and does not appear in the child's own lists.
	Parent   string   `json:"parent"`
	Examples []string `json:"examples"`
	// Operators are __call, __index and __len. Nine across the whole API, and
	// SEVEN OF THEM ARE ATTRIBUTE-SHAPED rather than method-shaped -- see Operator.
	Operators []Operator `json:"operators"`
}

// Operator is __call, __index or __len on a class.
//
// It gets its own type because the JSON gives it two different shapes and
// modelling it as a Method silently drops the more common one. __call carries
// parameters and return values; __index and __len carry a read_type, exactly
// like an attribute. Seven of the nine are the attribute form.
type Operator struct {
	Name         string        `json:"name"`
	Description  string        `json:"description"`
	Order        int           `json:"order"`
	Examples     []string      `json:"examples"`
	Format       Format        `json:"format"`
	Parameters   []Parameter   `json:"parameters"`
	ReturnValues []ReturnValue `json:"return_values"`
	ReadType     *Type         `json:"read_type"`
	Optional     bool          `json:"optional"`
}

// IsAttribute reports the attribute-shaped form (__index, __len).
func (o Operator) IsAttribute() bool { return o.ReadType != nil }

// Members is every callable and readable thing on a class. It is the number the
// project quotes -- 3782 across 157 classes at the 2.1.17 pin -- and operators
// are deliberately excluded from it, matching how Factorio's own documentation
// counts.
func (c Class) Members() int { return len(c.Methods) + len(c.Attributes) }

// Method is a class method, an operator, or a global function.
type Method struct {
	Name         string        `json:"name"`
	Description  string        `json:"description"`
	Order        int           `json:"order"`
	Parameters   []Parameter   `json:"parameters"`
	ReturnValues []ReturnValue `json:"return_values"`
	Format       Format        `json:"format"`
	// VariantGroups are extra parameters whose presence depends on an earlier
	// one's value. Exactly four methods have them -- LuaControl::set_gui_arrow,
	// LuaGuiElement::add, LuaSurface::create_entity and
	// LuaSurface::create_segmented_unit -- which is why the plan calls for
	// hand-writing those rather than generating them.
	VariantGroups []VariantGroup `json:"variant_parameter_groups"`
	VariantDesc   string         `json:"variant_parameter_description"`
	// Raises names the events calling this can cause the game to raise. It
	// matters for documentation, and for a guest that must not re-enter.
	Raises []Raise `json:"raises"`
	// Subclasses restricts the member to certain concrete classes. A member on
	// an abstract base that lists subclasses does NOT exist on every child, so a
	// generator that ignores this emits bindings that fail at runtime.
	Subclasses []string `json:"subclasses"`
	// VariadicParameter is the "..." tail, on the handful of methods that take one.
	VariadicParameter *Variadic `json:"variadic_parameter"`
	Examples          []string  `json:"examples"`
	// Lists are free-form documentation blocks. Carried so nothing is dropped.
	Lists []string `json:"lists"`
}

// Raise is one event a method can cause.
type Raise struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Order       int    `json:"order"`
	Timeframe   string `json:"timeframe"`
	Optional    bool   `json:"optional"`
}

// Variadic is a method's "..." tail.
type Variadic struct {
	Description string `json:"description"`
	Type        *Type  `json:"type"`
}

// TakesTable reports the calling convention: a table of named arguments rather
// than positional ones. 114 methods of 1039.
func (m Method) TakesTable() bool { return m.Format.TakesTable }

type Format struct {
	TakesTable bool `json:"takes_table"`
	// TableOptional means the argument table itself may be omitted, which is
	// only meaningful when every field in it is optional.
	TableOptional bool `json:"table_optional"`
}

// Attribute is a readable and/or writable property. ReadType is nil on a
// write-only attribute and WriteType nil on a read-only one, which is the
// common case.
type Attribute struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Order       int      `json:"order"`
	ReadType    *Type    `json:"read_type"`
	WriteType   *Type    `json:"write_type"`
	Optional    bool     `json:"optional"`
	Raises      []Raise  `json:"raises"`
	Subclasses  []string `json:"subclasses"`
	Examples    []string `json:"examples"`
	Lists       []string `json:"lists"`
}

type Parameter struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Order       int    `json:"order"`
	Type        Type   `json:"type"`
	Optional    bool   `json:"optional"`
}

type ReturnValue struct {
	Description string `json:"description"`
	Order       int    `json:"order"`
	Type        Type   `json:"type"`
	Optional    bool   `json:"optional"`
}

type VariantGroup struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Order       int         `json:"order"`
	Parameters  []Parameter `json:"parameters"`
}

// Event is something the game raises. 224 of them.
type Event struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Order       int         `json:"order"`
	Data        []Parameter `json:"data"`
	Examples    []string    `json:"examples"`
	// Filter names the concept describing this event's server-side filters,
	// empty when it cannot be filtered.
	Filter string `json:"filter"`
}

// Concept is a named type that is not a class: a table shape, a union, a string
// enum. `concepts` in census.json counts them, and they are the reason the
// marshalling layer needs more than one strategy.
type Concept struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Order       int      `json:"order"`
	Type        Type     `json:"type"`
	Examples    []string `json:"examples"`
	Lists       []string `json:"lists"`
}

// Define is a namespace of named integer constants, possibly nested.
//
// Their VALUES are Factorio's own and are not stable across versions, which is
// why generated code has to resolve them through a table at load rather than
// baking the numbers in.
type Define struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Order       int         `json:"order"`
	Values      []DefineVal `json:"values"`
	Subkeys     []Define    `json:"subkeys"`
}

type DefineVal struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Order       int    `json:"order"`
}

// GlobalObject is one of the nine globals a mod starts with: game, script,
// helpers, prototypes, rendering, settings, commands, remote, rcon.
//
// Note what is NOT here: `storage`. It is a plain table the engine serializes,
// not a LuaObject, so it has no class and needs no handle.
type GlobalObject struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Order       int    `json:"order"`
	Type        Type   `json:"type"`
}

// Type is a value type.
//
// The JSON encodes a plain named reference as a bare STRING ("string",
// "LuaEntity", "defines.events") and everything else as an object tagged with
// complex_type. UnmarshalJSON below is what reconciles those into one Go type,
// and it is the reason this file exists rather than a pile of map[string]any.
type Type struct {
	// Name is set when the JSON held a bare string, and is empty otherwise.
	Name string
	// Complex is the complex_type tag, and is empty for a bare name.
	Complex string
	// Description is carried only by the "type" wrapper, which exists purely to
	// attach one to a type that is otherwise a plain reference.
	Description string

	// Value is the element type of array, the wrapped type of `type` and
	// LuaLazyLoadedValue, and the value type of dictionary and LuaCustomTable.
	Value *Type
	// Key is the key type of dictionary and LuaCustomTable.
	Key *Type
	// Options are a union's alternatives.
	Options []Type
	// Values are a tuple's element types, in order.
	Values []Type
	// Parameters are a table's named fields.
	Parameters []Parameter
	// VariantGroups are a table's conditional fields. 55 concepts have them.
	VariantGroups []VariantGroup
	// FuncParams are a function type's parameter types. Kept apart from
	// Parameters because the JSON shape genuinely differs: a function's
	// parameters are BARE TYPE REFS, not named parameter objects.
	FuncParams []Type
	// Literal is a literal type's value: a string, number or bool.
	Literal any
	// Attributes are a LuaStruct's fields. Three in the whole API.
	Attributes []Attribute
	// VariantDesc documents a table's conditional fields.
	VariantDesc string
	// FullFormat is a documentation hint on a union: whether to render it
	// expanded. Carried so nothing is dropped.
	FullFormat bool
}

// IsNamed reports a plain reference to a named type rather than a structural one.
func (t Type) IsNamed() bool { return t.Complex == "" }

// String renders a type the way the API documentation writes it, which is what
// makes a generator's error messages and a diff report readable.
func (t Type) String() string {
	if t.IsNamed() {
		return t.Name
	}
	switch t.Complex {
	case "array":
		return t.Value.String() + "[]"
	case "dictionary", "LuaCustomTable":
		return t.Complex + "<" + t.Key.String() + ", " + t.Value.String() + ">"
	case "type", "LuaLazyLoadedValue":
		if t.Complex == "type" {
			return t.Value.String()
		}
		return "LuaLazyLoadedValue<" + t.Value.String() + ">"
	case "literal":
		return fmt.Sprintf("%v", t.Literal)
	case "union":
		s := ""
		for i, o := range t.Options {
			if i > 0 {
				s += " | "
			}
			s += o.String()
		}
		return s
	case "tuple":
		s := "["
		for i, v := range t.Values {
			if i > 0 {
				s += ", "
			}
			s += v.String()
		}
		return s + "]"
	}
	return t.Complex
}

// rawType mirrors the object form. Split out so UnmarshalJSON can decode into
// it without recursing into itself.
type rawType struct {
	ComplexType   string          `json:"complex_type"`
	Description   string          `json:"description"`
	Value         json.RawMessage `json:"value"`
	Key           *Type           `json:"key"`
	Options       []Type          `json:"options"`
	Values        []Type          `json:"values"`
	Parameters    json.RawMessage `json:"parameters"`
	VariantGroups []VariantGroup  `json:"variant_parameter_groups"`
	VariantDesc   string          `json:"variant_parameter_description"`
	Attributes    []Attribute     `json:"attributes"`
	FullFormat    bool            `json:"full_format"`
}

// UnmarshalJSON accepts either form.
//
// Two fields are polymorphic and both are handled explicitly rather than with
// an `any`, because getting them wrong is silent:
//
//   - `value` is a TYPE everywhere except on a literal, where it is the literal
//     itself -- a string, a number or a bool.
//   - `parameters` is a list of named parameter objects on a table, but a list
//     of bare type references on a function.
func (t *Type) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &t.Name)
	}
	// Strict here too. DisallowUnknownFields on the outer decoder does NOT
	// reach inside a custom UnmarshalJSON, so without its own decoder this type
	// would be the one hole in the guard -- and it is the recursive one, where
	// an unmodelled field is most likely to appear and least likely to be
	// noticed.
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var r rawType
	if err := dec.Decode(&r); err != nil {
		return err
	}
	t.Complex = r.ComplexType
	t.Description = r.Description
	t.Key = r.Key
	t.Options = r.Options
	t.Values = r.Values
	t.VariantGroups = r.VariantGroups
	t.VariantDesc = r.VariantDesc
	t.Attributes = r.Attributes
	t.FullFormat = r.FullFormat

	if len(r.Value) > 0 {
		if r.ComplexType == "literal" {
			if err := json.Unmarshal(r.Value, &t.Literal); err != nil {
				return fmt.Errorf("literal value: %w", err)
			}
		} else {
			var v Type
			if err := json.Unmarshal(r.Value, &v); err != nil {
				return fmt.Errorf("%s value: %w", r.ComplexType, err)
			}
			t.Value = &v
		}
	}
	if len(r.Parameters) > 0 {
		if r.ComplexType == "function" {
			if err := json.Unmarshal(r.Parameters, &t.FuncParams); err != nil {
				return fmt.Errorf("function parameters: %w", err)
			}
		} else if err := json.Unmarshal(r.Parameters, &t.Parameters); err != nil {
			return fmt.Errorf("%s parameters: %w", r.ComplexType, err)
		}
	}
	return nil
}

// DefaultAPIVersion is the Factorio release a project with no `api` pin builds
// against: the description under api/<version>/ that the committed bindings
// were generated from, and the member table `fklua mod` attaches. Several
// versions ship in api/; `fklua.toml`'s `api` and `--api=VERSION` select
// another one.
//
// Moving this constant is a THREE-PART change and the other two are not
// optional. The bindings must be regenerated (`fklua gen-bindings --lang=all`,
// gated by `--check`), because a member id is a dense sorted index per version
// and a member added or removed anywhere shifts every later one — guest
// bindings from one description against a member table from another call the
// WRONG member, silently. And DefaultFactorioVersion, the series a packaged
// mod declares in info.json, is DERIVED from this string rather than written
// out beside it; see mod.go for why. The whole checklist, distilled from the
// two migrations that have actually been performed, is in
// agents/versioning.md, "Moving the default pin".
//
// IT IS THE GENERAL-AVAILABILITY RELEASE, NOT THE NEWEST DESCRIPTION IN api/
// AND NOT WHAT IS INSTALLED ON THE MACHINE THAT BUILT THIS. A default is what
// a mod author who has pinned nothing ships to players, and players are on
// stable. 2.1.x is available to anyone who wants it -- `api = "2.1.17"` in
// fklua.toml, or `--api=2.1.17` -- and every description under api/ is
// committed precisely so that choice needs neither the game nor the network.
//
// THIS IS A BUILD-TIME AXIS AND IT IS NOT THE ENGINE A MOD RUNS ON. The two
// are independent and conflating them has cost this project a gate already:
//
//   - The PIN decides which description the bindings and the packaged member
//     table come from. It is fixed when the mod is built.
//   - The ENGINE is whatever Factorio the player launches. A guest that wants
//     to know reads it at RUN TIME (helpers.game_version), which is what
//     fkipc's version gate does -- so a GA-pinned mod gets the full IPC
//     library on a 2.1.17 engine with no rebuild and no repin.
//
// The one place the two axes MEET is info.json's factorio_version, which is a
// statement about the ENGINE and defaults to this constant's series only
// because a mod built against a description usually runs on that series. It is
// overridable -- `[mod] factorio_version` and `--factorio-version` -- and every
// in-game gate in scripts/ overrides it with the INSTALLED engine's series, for
// the reason DefaultFactorioVersion's own comment gives.
const DefaultAPIVersion = "2.0.77"

// ParseAPI decodes a runtime-api.json.
//
// DisallowUnknownFields is deliberate. The schema is versioned and stable, so a
// field we do not model is news: either Factorio added something, or this
// parser is wrong about a shape. Both are things to find at parse time rather
// than by noticing a binding is missing later.
func ParseAPI(r io.Reader) (*API, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var a API
	if err := dec.Decode(&a); err != nil {
		return nil, fmt.Errorf("runtime-api.json: %w", err)
	}
	if a.APIVersion == 0 {
		return nil, fmt.Errorf("runtime-api.json: no api_version; is this a runtime API dump?")
	}
	return &a, nil
}

// LoadAPI reads and parses a runtime-api.json from disk.
func LoadAPI(path string) (*API, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	a, err := ParseAPI(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return a, nil
}

// Members counts every method and attribute across every class.
func (a *API) Members() int {
	n := 0
	for _, c := range a.Classes {
		n += c.Members()
	}
	return n
}
