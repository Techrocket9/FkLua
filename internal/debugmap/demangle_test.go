package debugmap

import "testing"

// The v0 symbols here are REAL: every one was read out of the name section of a
// wasm built by the scaffold this compiler writes, at the flags it writes them
// with. A demangler tested only against symbols someone made up is a demangler
// tested against its own author's model of the scheme.
func TestDemangle(t *testing.T) {
	for _, tc := range []struct {
		what    string
		sym     string
		name    string
		mangled string
	}{
		{
			"a crate-level function",
			"_RNvCs2jHPn97boMd_14tiera_rs_guest8push_u32",
			"tiera_rs_guest::push_u32", "_RNvCs2jHPn97boMd_14tiera_rs_guest8push_u32",
		},
		{
			"a module path inside a crate",
			"_RNvNtCs1AJuqHJ3rjd_4fkgc7collect9mark_step",
			"fkgc::collect::mark_step", "_RNvNtCs1AJuqHJ3rjd_4fkgc7collect9mark_step",
		},
		{
			"a standard library path",
			"_RNvNtCsdkdt1aaAg1T_4core9panicking18panic_bounds_check",
			"core::panicking::panic_bounds_check",
			"_RNvNtCsdkdt1aaAg1T_4core9panicking18panic_bounds_check",
		},
		{
			"three components deep",
			"_RNvNtNtCsdkdt1aaAg1T_4core5slice5index16slice_index_fail",
			"core::slice::index::slice_index_fail",
			"_RNvNtNtCsdkdt1aaAg1T_4core5slice5index16slice_index_fail",
		},
		{
			// An impl path needs a type renderer to say anything true, so the
			// symbol is kept. This is the degradation, and it is deliberate:
			// the raw symbol is ugly and correct.
			"an impl method is left alone",
			"_RNvMsJ_NtNtNtCsewWLk9TkM7w_5alloc11collections5btree4node10insert_fit",
			"_RNvMsJ_NtNtNtCsewWLk9TkM7w_5alloc11collections5btree4node10insert_fit", "",
		},
		{
			"a generic instantiation renders as its path",
			"_RINvNtCsewWLk9TkM7w_5alloc7raw_vec12handle_errorlE",
			"alloc::raw_vec::handle_error", "_RINvNtCsewWLk9TkM7w_5alloc7raw_vec12handle_errorlE",
		},
		{
			// #[no_mangle], which is every FkLua entry point in a Rust guest.
			"an unmangled export",
			"fk_on_tick", "fk_on_tick", "",
		},
		{
			"a Go symbol",
			"main.onTick#wasmexport", "main.onTick#wasmexport", "",
		},
		{
			"FkLua's own synthetic name",
			"func[142]", "func[142]", "",
		},
		{
			"the legacy scheme, hash dropped",
			"_ZN4core3fmt5write17h0123456789abcdefE",
			"core::fmt::write", "_ZN4core3fmt5write17h0123456789abcdefE",
		},
		{
			"a legacy symbol with no hash keeps every component",
			"_ZN3foo3barE", "foo::bar", "_ZN3foo3barE",
		},
		{"the prefix and nothing else", "_R", "_R", ""},
		{"a length that runs off the end", "_RNvC9tooshort", "_RNvC9tooshort", ""},
		{"a length of zero", "_RNvC0", "_RNvC0", ""},
		{"an unterminated disambiguator", "_RNvCs123", "_RNvCs123", ""},
		{"a path form this does not model", "_RYNtC4test5Thing", "_RYNtC4test5Thing", ""},
		{"a bare underscore", "_", "_", ""},
		{"nothing at all", "", "", ""},
	} {
		t.Run(tc.what, func(t *testing.T) {
			name, mangled := Demangle(tc.sym)
			if name != tc.name {
				t.Errorf("name = %q, want %q", name, tc.name)
			}
			if mangled != tc.mangled {
				t.Errorf("mangled = %q, want %q", mangled, tc.mangled)
			}
		})
	}
}

// The second result is the map's `mangled` field, and it is present ONLY when
// something changed. A demangler that echoed the input into it would double the
// size of every Rust map for nothing.
func TestTheRawSymbolIsReportedOnlyWhenItChanged(t *testing.T) {
	for _, sym := range []string{"memcpy", "main.main", "func[7]", "_RNvC4halt"} {
		if _, mangled := Demangle(sym); mangled != "" {
			t.Errorf("%q was reported as demangled from %q", sym, mangled)
		}
	}
	name, mangled := Demangle("_RNvCs2jHPn97boMd_14tiera_rs_guest8push_u32")
	if mangled == "" || mangled == name {
		t.Errorf("a demangled symbol must carry its raw form: %q / %q", name, mangled)
	}
}

// Whatever it decides, it never invents a name out of bytes that were not one.
// A mis-parse that produced a plausible identifier would put a wrong function
// name in a stack frame, which is the one failure this whole file exists to
// avoid.
func TestDemanglingNeverInventsPunctuation(t *testing.T) {
	for _, sym := range []string{
		"_RNvC4te\x00t", "_RNvNvNvNvNvNvC1a1b1c1d1e1f", "_RC10abcdefghij",
		"_RNvCs_1a", "_RIIIIIIIC1a", "_ZN1a" + "\xff\xfe" + "E",
	} {
		name, _ := Demangle(sym)
		if name == sym {
			continue
		}
		for i := 0; i < len(name); i++ {
			c := name[i]
			ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
				c >= '0' && c <= '9' || c == '_' || c == ':'
			if !ok {
				t.Errorf("%q demangled to %q, which contains %q", sym, name, string(c))
				break
			}
		}
	}
}
