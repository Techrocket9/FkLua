//! Guest-side half of Factorio's SETTINGS and DATA stages, for Rust.
//!
//! A data guest is a second wasm module, compiled from its own crate, packaged
//! beside the control guest and run by the stage files `fklua mod` generates.
//! It writes ordinary `no_std` Rust, marks its entry points with
//! `#[no_mangle] pub extern "C"`, and reaches `data.raw` through the seven
//! imports declared here:
//!
//! ```ignore
//! #[no_mangle]
//! pub extern "C" fn fk_data() {
//!     fkdata::extend(&[fkdata::obj(&[
//!         ("type", fkdata::str_("item")),
//!         ("name", fkdata::str_("my-item")),
//!         ("stack_size", fkdata::num(50.0)),
//!     ])]);
//! }
//! ```
//!
//! THIS IS A LINE-FOR-LINE MIRROR OF `guest/go/fkdata`, and it is one for a
//! reason this repository has already paid for once: the Rust generator fell
//! four milestones behind the Go one, every gap was reported by a mod author
//! rather than found here, and one of them was the same defect in the same
//! function the Go side had already fixed. `fkdata` is hand-written, so
//! `census.json` cannot see it at all -- which makes a Go-only round strictly
//! worse than the generated case that went wrong. The precedent is `fkipc`,
//! which shipped in both languages at once with one test requiring identical
//! wire behaviour from the two example guests, and that is what
//! `TestBothDataGuestLibrariesMakeTheSameCalls` does here.
//!
//! # The four hooks
//!
//! One export per stage, and `fklua mod` generates a stage file only for the
//! exports the module actually has:
//!
//! ```text
//! fk_settings           -> settings.lua
//! fk_data               -> data.lua
//! fk_data_updates       -> data-updates.lua
//! fk_data_final_fixes   -> data-final-fixes.lua
//! ```
//!
//! # Do not depend on `fkapi` from a data guest
//!
//! There is no runtime API at these stages: no `game`, no `script`, no
//! `storage`, no entities and no events. `fklua mod` refuses a data module
//! that imports anything but `fkdata` and `env`, and names this.
//!
//! # No state survives a stage
//!
//! This module is instantiated FRESH for each stage it hooks: Factorio's
//! settings stage is its own Lua state, the three data stages share one, and
//! `require` re-executes a file at every stage. A `static` set in `fk_data` is
//! back at its initial value in `fk_data_updates`. The place to keep something
//! between stages is `data.raw`.
//!
//! # Determinism
//!
//! The data stage runs per client and a divergent prototype set is a JOIN
//! REFUSAL, so nothing here hands a guest an iteration order it could branch
//! on: [`keys`] is sorted, [`mods`] is sorted, and every dictionary a [`get`]
//! returns is sorted by key at every nesting level. That is a property of the
//! host shim rather than a rule to follow.
//!
//! # Strings are BYTES, and nothing here rewrites one
//!
//! A Lua string is an arbitrary byte sequence, and so is everything that
//! crosses this ABI as a string: a stored setting value the engine hands
//! through unchanged, a prototype field somebody else's mod wrote, a path
//! element. **Every string crosses byte-exact in both directions**, and neither
//! this library nor its Go twin ever alters one.
//!
//! Rust's `String` cannot hold that, so [`V::Str`] holds a [`LuaStr`], which
//! owns a `Vec<u8>`. [`V::bytes`] is the bytes, always; [`V::string`] is the
//! CHECKED conversion and answers `None` for a string that is not UTF-8. The
//! Go twin has no such split, because a Go `string` is already a byte string.
//!
//! The one place this library refuses rather than handing bytes through is the
//! handful of names the ENGINE constrains: [`mods`], [`mod_version`],
//! [`mod_name`], [`feature_flags`], [`derived_types`] and [`base_type`] answer
//! with `String`, and a value there that is not UTF-8 stops the load through
//! [`raise`] with the surface named and the bytes in hex. Rewriting it lossily
//! would change its length silently, which is the defect this contract exists
//! to prevent.

#![no_std]

extern crate alloc;

use alloc::borrow::Cow;
use alloc::format;
use alloc::string::String;
use alloc::vec::Vec;

// ---------------------------------------------------------------------------
// The imports.
// ---------------------------------------------------------------------------

#[link(wasm_import_module = "fkdata")]
extern "C" {
    #[link_name = "stage"]
    fn fkd_stage() -> u32;
    #[link_name = "get"]
    fn fkd_get(pathp: u32, retp: u32) -> u32;
    #[link_name = "set"]
    fn fkd_set(pathp: u32, valp: u32) -> u32;
    #[link_name = "extend"]
    fn fkd_extend(valp: u32) -> u32;
    #[link_name = "clone"]
    fn fkd_clone(pathp: u32, dstp: u32) -> u32;
    #[link_name = "keys"]
    fn fkd_keys(pathp: u32, retp: u32) -> u32;
    #[link_name = "env"]
    fn fkd_env(which: u32, retp: u32) -> u32;
    // The only import with an off-target caller that is stubbed out rather than
    // gated away, so it is the only one the host build sees as unused.
    #[cfg_attr(not(target_family = "wasm"), allow(dead_code))]
    #[link_name = "raise"]
    fn fkd_raise(ptr: u32, len: u32);
}

/// Everything that is a FAILURE raises at the stage instead of arriving here,
/// so there are two statuses.
const STATUS_ABSENT: u32 = 1;

/// Which of the four stages a call is running in. The numbers are the ABI.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum StageId {
    Settings,
    Data,
    DataUpdates,
    DataFinalFixes,
    Unknown,
}

impl StageId {
    /// The stage's name as Factorio spells its file.
    pub fn name(self) -> &'static str {
        match self {
            StageId::Settings => "settings",
            StageId::Data => "data",
            StageId::DataUpdates => "data-updates",
            StageId::DataFinalFixes => "data-final-fixes",
            StageId::Unknown => "unknown",
        }
    }
}

/// Which stage is running.
pub fn stage() -> StageId {
    match unsafe { fkd_stage() } {
        1 => StageId::Settings,
        2 => StageId::Data,
        3 => StageId::DataUpdates,
        4 => StageId::DataFinalFixes,
        _ => StageId::Unknown,
    }
}

/// Writes a line to `factorio-current.log`, which is the only output channel a
/// data stage has: there is no console, because there is no game yet.
#[cfg(target_family = "wasm")]
pub fn log(s: &str) {
    fk::log(s)
}

/// Off-target there is no game and no log. See `Cargo.toml`: a host build of
/// this crate exists so the value model and the byte type have native tests,
/// and the wire is guest-only by construction.
#[cfg(not(target_family = "wasm"))]
pub fn log(_s: &str) {}

/// Stops the load with THIS GUEST'S OWN message, exactly as a host-detected
/// failure does: the stage name is prefixed, the error unwinds the whole
/// stage, and the call never returns.
///
/// The host puts `fklua: at the <stage> stage, ` in front of the message,
/// verbatim, and nothing else, so a message must NOT name the stage itself
/// or the player reads it twice. `raise("two technologies share the name
/// hardened-tips")` at the data stage reads, in the player's game:
///
/// ```text
/// fklua: at the data stage, two technologies share the name hardened-tips
/// ```
///
/// This is what a validating guest uses instead of panicking. A panic
/// surfaces in the player's game as `fklua trap: unreachable`, with whatever
/// diagnostic was built surviving only as a log line above it; `raise` puts
/// the message where the player looks. A cycle detector naming its path, a
/// presence check naming the missing prototype, a refused configuration
/// naming the setting: this is their exit.
#[cfg(target_family = "wasm")]
pub fn raise(msg: &str) -> ! {
    unsafe { fkd_raise(msg.as_ptr() as u32, msg.len() as u32) };
    // The host raise unwinds the Lua stage through this call, so execution
    // never resumes here. Under a stand-in that failed to raise, stopping
    // hard beats running on past a load that should have been refused.
    unreachable!("fkdata: raise returned")
}

/// Off-target there is no stage to unwind, so the contract that is left is the
/// one the signature states: it does not return, and it carries the message.
#[cfg(not(target_family = "wasm"))]
pub fn raise(msg: &str) -> ! {
    panic!("{msg}")
}

// ---------------------------------------------------------------------------
// The value model.
// ---------------------------------------------------------------------------

/// A LUA STRING, WHICH IS A BYTE STRING AND NOT UTF-8.
///
/// The data-stage contract is that every string crosses this ABI byte-exact in
/// both directions, and Rust's `String` cannot express that: it carries a
/// validity invariant that safe code relies on. This library used to reconcile
/// the two with `String::from_utf8_lossy`, which turns every byte outside UTF-8
/// into U+FFFD, silently, and changes the length while it does it -- so the Go
/// half and the Rust half saw different strings and different lengths for one
/// wire. A stored setting value is where that reaches a real mod: the engine
/// carries invalid UTF-8 and control characters through `mod-settings.dat`
/// unchanged, so what a player typed is what a guest must be able to read back.
/// (A NUL truncates the stored value inside the engine, which is the engine's
/// behaviour and not this ABI's. A NUL that does reach this type crosses it
/// whole, because a Lua string is counted rather than terminated.)
///
/// So the byte string gets a type, the same one the generated runtime-API
/// bindings carry under this name. It is DECLARED HERE rather than imported,
/// because a data module that imports `fkapi` is refused at package time.
///
/// `from_utf8_unchecked` was the cheap alternative and it is not available:
/// handing safe Rust a `String` whose bytes are not UTF-8 is library UB the
/// moment anything slices, iterates or formats it, and these bytes come from
/// the engine rather than from the guest.
///
/// `Ord` and `Hash` are byte-lexicographic, which is the order `fk_data.lua`
/// sorts a map's keys with and the order the Go twin's `string` comparison
/// gives, so no dictionary's wire order moves.
#[derive(Clone, Default, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct LuaStr(Vec<u8>);

impl LuaStr {
    #[inline]
    pub fn new() -> Self {
        LuaStr(Vec::new())
    }

    /// The bytes, always.
    #[inline]
    pub fn as_bytes(&self) -> &[u8] {
        &self.0
    }

    #[inline]
    pub fn into_bytes(self) -> Vec<u8> {
        self.0
    }

    /// The bytes as text, or `None` when they are not UTF-8. THE CHECKED
    /// CONVERSION: a caller that needs a `&str` and cares whether it got one
    /// asks for it here.
    #[inline]
    pub fn as_str(&self) -> Option<&str> {
        core::str::from_utf8(&self.0).ok()
    }

    /// The lossy conversion, NAMED. Every byte outside UTF-8 becomes U+FFFD and
    /// the length changes with it, which is fine for a log line and wrong for
    /// anything that has to go back out to the engine.
    #[inline]
    pub fn to_string_lossy(&self) -> Cow<'_, str> {
        String::from_utf8_lossy(&self.0)
    }

    /// Replaces the contents, REUSING the allocation where the capacity allows.
    #[inline]
    pub fn set(&mut self, b: &[u8]) {
        self.0.clear();
        self.0.extend_from_slice(b);
    }

    #[inline]
    pub fn clear(&mut self) {
        self.0.clear();
    }
}

impl core::ops::Deref for LuaStr {
    type Target = [u8];
    #[inline]
    fn deref(&self) -> &[u8] {
        &self.0
    }
}

impl AsRef<[u8]> for LuaStr {
    #[inline]
    fn as_ref(&self) -> &[u8] {
        &self.0
    }
}

/// `Borrow<[u8]>` and NOT `Borrow<str>`, which is what a caller would rather
/// write. `Borrow` is infallible and Ord-consistent by contract, and a `LuaStr`
/// holding bytes that are not UTF-8 has no `&str` to hand back.
impl core::borrow::Borrow<[u8]> for LuaStr {
    #[inline]
    fn borrow(&self) -> &[u8] {
        &self.0
    }
}

impl From<&str> for LuaStr {
    #[inline]
    fn from(s: &str) -> Self {
        LuaStr(s.as_bytes().to_vec())
    }
}

impl From<String> for LuaStr {
    #[inline]
    fn from(s: String) -> Self {
        LuaStr(s.into_bytes())
    }
}

impl From<&[u8]> for LuaStr {
    #[inline]
    fn from(b: &[u8]) -> Self {
        LuaStr(b.to_vec())
    }
}

impl From<Vec<u8>> for LuaStr {
    #[inline]
    fn from(b: Vec<u8>) -> Self {
        LuaStr(b)
    }
}

impl PartialEq<str> for LuaStr {
    #[inline]
    fn eq(&self, other: &str) -> bool {
        self.0 == other.as_bytes()
    }
}

impl PartialEq<&str> for LuaStr {
    #[inline]
    fn eq(&self, other: &&str) -> bool {
        self.0 == other.as_bytes()
    }
}

impl PartialEq<[u8]> for LuaStr {
    #[inline]
    fn eq(&self, other: &[u8]) -> bool {
        self.0 == other
    }
}

/// LOSSY, deliberately and visibly. Formatting is the text case, and a byte
/// string that is not text has no rendering; what a caller must not be able to
/// do is round-trip through here by accident, which is why the bytes come back
/// out of [`LuaStr::as_bytes`] and never out of a formatter.
impl core::fmt::Display for LuaStr {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        core::fmt::Display::fmt(&self.to_string_lossy(), f)
    }
}

impl core::fmt::Debug for LuaStr {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        core::fmt::Debug::fmt(&self.to_string_lossy(), f)
    }
}

/// One dynamic value: nil, a bool, a number, a string, an array or a map.
///
/// Tier 2's model, which is the codec `fk_abi.lua` already has and which is
/// measured loadable at both stages. NOT a generated per-prototype type model:
/// there are 251 prototype types, the description that would drive a generator
/// is `prototype-api.json` rather than `runtime-api.json`, and every operation
/// here is a read, a write, an extend or a clone of an untyped structure.
#[derive(Clone, Debug, Default, PartialEq)]
pub enum V {
    #[default]
    Nil,
    Bool(bool),
    Num(f64),
    /// A byte string, not a `String`: see [`LuaStr`]. `str_("item")` and
    /// `bytes_(b"\xff\x00")` both build one, and it crosses byte-exact.
    Str(LuaStr),
    /// A LuaObject. Never produced at a data stage: the handle table is the
    /// control stage's and `fk_data.lua` does not bind it.
    Obj(u32),
    Arr(Vec<V>),
    /// A Vec of pairs rather than a map, for the reason the Go side gives:
    /// an iteration order a guest could branch on is exactly what the host's
    /// sorting exists to remove. The pairs arrive sorted by key.
    Map(Vec<(V, V)>),
}

/// A number.
pub fn num(f: f64) -> V {
    V::Num(f)
}

/// A string, from text. Its BYTES are what crosses, so this is byte-exact for
/// everything a `&str` can hold; [`bytes_`] is how to say the rest.
pub fn str_(s: &str) -> V {
    V::Str(LuaStr::from(s))
}

/// A string, from bytes. A Lua string is an arbitrary byte sequence, so this is
/// the general constructor and [`str_`] is the convenient one.
pub fn bytes_(b: &[u8]) -> V {
    V::Str(LuaStr::from(b))
}

/// A boolean.
pub fn bool_(b: bool) -> V {
    V::Bool(b)
}

/// An array, which is what Lua calls a table with 1..n and nothing else.
pub fn arr(vs: &[V]) -> V {
    V::Arr(vs.to_vec())
}

/// A map, in the order given. Prototype fields are what this is for.
pub fn obj(kvs: &[(&str, V)]) -> V {
    V::Map(
        kvs.iter()
            .map(|(k, v)| (V::Str(LuaStr::from(*k)), v.clone()))
            .collect(),
    )
}

impl V {
    /// Reads a numeric value, or 0.
    pub fn number(&self) -> f64 {
        match self {
            V::Num(n) => *n,
            _ => 0.0,
        }
    }

    /// Reads a string value's BYTES, or an empty slice for anything that is not
    /// a string.
    ///
    /// This is the byte-exact twin of the Go half's `V.String()`, and it is
    /// what to read when the value is carried rather than inspected: a stored
    /// setting the engine hands through unchanged, a prototype field another
    /// mod wrote. Nothing between the engine and here alters a byte.
    pub fn bytes(&self) -> &[u8] {
        match self {
            V::Str(s) => s.as_bytes(),
            _ => &[],
        }
    }

    /// Reads a string value as TEXT, and `None` when there is no text to read:
    /// the value is not a string, or its bytes are not valid UTF-8.
    ///
    /// The `Option` is deliberate. A Lua string is an arbitrary byte sequence,
    /// so the conversion can fail, and the type is what makes every caller
    /// decide at compile time whether it wanted text or bytes. [`V::bytes`] is
    /// the answer that never fails.
    pub fn string(&self) -> Option<&str> {
        match self {
            V::Str(s) => s.as_str(),
            _ => None,
        }
    }

    /// Reads a boolean value, or false.
    pub fn boolean(&self) -> bool {
        matches!(self, V::Bool(true))
    }

    /// Looks one key up in a map. `None` for anything that is not a map, and
    /// for a key the map does not have.
    ///
    /// The comparison is over BYTES, and so is the ARGUMENT: `at("name")` and
    /// `at(&[0xff, 0xfe][..])` are the same call, because a prototype field is
    /// a key in a table any mod can write and a byte comparison no caller can
    /// reach is not a byte comparison. A `&str` parameter would have left this
    /// doc comment true and unreachable, which is how the defect this whole
    /// contract answers looked from the outside.
    pub fn at<K: AsRef<[u8]>>(&self, key: K) -> Option<&V> {
        let key = key.as_ref();
        match self {
            V::Map(pairs) => pairs
                .iter()
                .find(|(k, _)| matches!(k, V::Str(_)) && k.bytes() == key)
                .map(|(_, v)| v),
            _ => None,
        }
    }

    /// The number of elements in an array, or of pairs in a map.
    pub fn len(&self) -> usize {
        match self {
            V::Arr(items) => items.len(),
            V::Map(pairs) => pairs.len(),
            _ => 0,
        }
    }

    /// Whether an array or map has nothing in it.
    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }
}

/// One element of a path: a prototype type, a prototype name, a field name, or
/// an array index.
///
/// `S` is text and `B` is the same position as BYTES. A path element is a
/// string crossing into the engine like any other, and a prototype name is a
/// key any mod can write, so the name [`keys`] hands back as a [`LuaStr`] has
/// to be spellable on the way back in: `P::B(name.as_bytes())`, or
/// `(&name).into()`. Without it the contract held for every string but this
/// one, and the Go twin needed no second form only because a Go string is
/// already bytes.
#[derive(Clone, Debug)]
pub enum P<'a> {
    S(&'a str),
    B(&'a [u8]),
    N(f64),
}

impl<'a> From<&'a str> for P<'a> {
    fn from(s: &'a str) -> Self {
        P::S(s)
    }
}

impl<'a> From<&'a [u8]> for P<'a> {
    fn from(b: &'a [u8]) -> Self {
        P::B(b)
    }
}

impl<'a> From<&'a LuaStr> for P<'a> {
    fn from(s: &'a LuaStr) -> Self {
        P::B(s.as_bytes())
    }
}

impl From<usize> for P<'static> {
    fn from(n: usize) -> Self {
        P::N(n as f64)
    }
}

// ---------------------------------------------------------------------------
// The operations
// ---------------------------------------------------------------------------

/// Reads one value out of `data.raw`, at any depth.
///
/// ```ignore
/// let count = fkdata::get(&["technology".into(), "logistics".into(),
///                           "unit".into(), "count".into()]);
/// ```
///
/// `None` means the path is not there, which is a NORMAL answer rather than an
/// error: "is this prototype already defined" is what a mod adopting another
/// mod's entities asks on every load. Anything that really is a failure raises
/// at the stage with the stage name and the path in the message.
///
/// A dictionary comes back with its pairs SORTED BY KEY, at every level.
pub fn get(path: &[P]) -> Option<V> {
    let p = encode_path(path);
    let ret = slot();
    if unsafe { fkd_get(p, ret) } == STATUS_ABSENT {
        return None;
    }
    Some(read_dyn_at(ret))
}

/// Writes one value into `data.raw`, at any depth.
///
/// Setting [`V::Nil`] DELETES the key, which is not decoration: stripping a
/// cloned prototype is a list of deletions, and a "write false" reading of an
/// absent value would leave those fields present-and-false in the prototype.
///
/// An intermediate step that is not there raises rather than being created.
pub fn set(value: &V, path: &[P]) {
    let p = encode_path(path);
    if matches!(value, V::Nil) {
        unsafe { fkd_set(p, 0) };
        return;
    }
    let v = slot();
    write_dyn_at(v, value);
    unsafe { fkd_set(p, v) };
}

/// `data:extend`: adds prototypes.
///
/// Factorio's own extend is the validator. A prototype with no type or no name
/// is refused by the game, by name, which is a better message than anything
/// this layer could invent.
pub fn extend(protos: &[V]) {
    if protos.is_empty() {
        return;
    }
    let v = slot();
    write_dyn_at(v, &V::Arr(protos.to_vec()));
    unsafe { fkd_extend(v) };
}

/// Deep-copies one prototype under another name, within one type.
///
/// THE COPY IS THE ENGINE'S OWN `util.table.deepcopy`, and that is the whole
/// reason this is a primitive rather than a get plus an extend. Reading a
/// prototype into the guest and writing it back re-serialises every leaf, so
/// any field tier 2 cannot express, any float that does not round-trip and any
/// key this value model drops would change the prototype SILENTLY while the mod
/// still loads.
///
/// Patch the copy afterwards with [`set`].
pub fn clone_(typ: &str, from: &str, to: &str) {
    clone_to(typ, from, typ, to)
}

/// [`clone_`] across prototype types.
pub fn clone_to(src_type: &str, src_name: &str, dst_type: &str, dst_name: &str) {
    let src = encode_path(&[P::S(src_type), P::S(src_name)]);
    let dst = encode_path(&[P::S(dst_type), P::S(dst_name)]);
    unsafe { fkd_clone(src, dst) };
}

/// The STRING keys at a path, SORTED.
///
/// This is the deterministic enumeration primitive, and the sort is why: the
/// engine's own iteration order over `data.raw` is insertion order, which is a
/// fact about how the mods happened to load rather than a promise this ABI may
/// make. A tie broken by iteration order is a prototype set that differs
/// between clients, which Factorio answers with a join refusal.
///
/// Numeric keys are not string keys and are not returned; read an array with
/// [`get`].
///
/// A prototype name is a key in a global namespace any mod can write, so the
/// keys come back as [`LuaStr`] rather than as `String`: byte-exact, and a name
/// that is not UTF-8 is enumerable rather than dropped. `k.as_str()` is the
/// checked conversion where text is what is wanted.
pub fn keys(path: &[P]) -> Vec<LuaStr> {
    let p = encode_path(path);
    let ret = slot();
    if unsafe { fkd_keys(p, ret) } == STATUS_ABSENT {
        return Vec::new();
    }
    match read_dyn_at(ret) {
        V::Arr(items) => items
            .into_iter()
            .filter_map(|k| match k {
                V::Str(s) => Some(s),
                _ => None,
            })
            .collect(),
        _ => Vec::new(),
    }
}

/// Every installed mod and its version, SORTED BY NAME.
///
/// `String` rather than [`LuaStr`], because the engine constrains both: see
/// [`text`]'s note on the one place this library refuses.
pub fn mods() -> Vec<(String, String)> {
    match env_value(1) {
        V::Map(pairs) => pairs
            .iter()
            .map(|(k, v)| (text("a mod name", k), text("a mod version", v)))
            .collect(),
        _ => Vec::new(),
    }
}

/// One mod's version, and whether it is installed at all.
pub fn mod_version(name: &str) -> Option<String> {
    env_value(1).at(name).map(|v| text("a mod version", v))
}

/// Reads one of the engine's feature flags, such as `space_travel` or
/// `quality`.
pub fn feature_flag(name: &str) -> bool {
    env_value(2).at(name).map(|v| v.boolean()).unwrap_or(false)
}

/// Every feature flag, SORTED BY NAME.
pub fn feature_flags() -> Vec<(String, bool)> {
    match env_value(2) {
        V::Map(pairs) => pairs
            .iter()
            .map(|(k, v)| (text("a feature flag name", k), v.boolean()))
            .collect(),
        _ => Vec::new(),
    }
}

/// Reads one startup setting's VALUE, unwrapped from the `{value = ...}` table
/// the engine keeps it in.
///
/// `None` when there is no such setting, and `None` for EVERY setting at the
/// settings stage, where `settings` does not exist at all because a mod's
/// startup settings are not readable while they are being declared.
pub fn startup_setting(name: &str) -> Option<V> {
    env_value(3).at(name).cloned()
}

/// This mod's OWN name, exactly as `fklua mod` packaged it.
///
/// THE PACKAGER SUPPLIES IT, NOT THE ENGINE. The data-stage environment has no
/// "current mod" anywhere -- [`mods`] is a flat all-mods dictionary with no
/// self marker, and `script.mod_name` is runtime-only -- so `fklua mod` writes
/// the manifest's name into the generated stage file's `run()` call, which is
/// authoritative because the packager is what wrote info.json.
///
/// What it is FOR is namespacing. Settings and prototypes share GLOBAL
/// namespaces, and a same-type name collision between two mods is silent
/// last-writer-wins in the engine, so anything a mod (or a library inside one)
/// generates should be prefixed -- and this is the prefix's one source that
/// cannot drift from the packaged mod.
///
/// Empty under a stage file written by an fklua older than the argument.
pub fn mod_name() -> String {
    text("the mod's own name", &env_value(4))
}

/// Every concrete prototype TYPE under one of the engine's base types, SORTED
/// -- the engine's `defines.prototypes["item"]` holds `ammo`, `armor`, `tool`
/// and the rest.
///
/// This is the enumeration a prototype browser is built on: "every kind of
/// item prototype" is a question `data.raw` alone cannot answer, because a
/// concrete type carries no marker saying which base type it narrows. Empty
/// for a name that is not a base type.
pub fn derived_types(base: &str) -> Vec<String> {
    match env_value(5).at(base) {
        Some(V::Arr(items)) => items
            .iter()
            .filter_map(|d| match d {
                V::Str(_) => Some(text("a derived prototype type", d)),
                _ => None,
            })
            .collect(),
        _ => Vec::new(),
    }
}

/// The base prototype type a concrete TYPE derives from --
/// `base_type("transport-belt")` answers `entity` -- and `None` for a name the
/// engine's `defines.prototypes` lists nowhere.
///
/// Should the engine ever list one name under more than one base, the first
/// base in sorted order answers, deterministically.
pub fn base_type(derived: &str) -> Option<String> {
    if let V::Map(pairs) = env_value(5) {
        for (k, v) in pairs.iter() {
            if let V::Arr(items) = v {
                if items
                    .iter()
                    .any(|d| matches!(d, V::Str(_)) && d.bytes() == derived.as_bytes())
                {
                    return Some(text("a base prototype type", k));
                }
            }
        }
    }
    None
}

/// The ONE PLACE THIS LIBRARY REFUSES A STRING RATHER THAN CARRYING IT.
///
/// The conveniences above answer with `String` because the engine constrains
/// what they name: a mod name, a mod version, a feature flag, a prototype type.
/// If one of those is ever not UTF-8 the honest answers are to refuse or to
/// widen the type, and refusing is the one that cannot corrupt anything: a
/// lossy rewrite would change the value's length silently, and this ABI's whole
/// contract is that it does not do that. The message names the surface and
/// prints the bytes in hex, so the offending value is legible in the log rather
/// than being whatever a terminal makes of it.
///
/// A non-string value answers with the empty string, which is what an absent
/// [`mod_name`] is.
fn text(surface: &str, v: &V) -> String {
    match v {
        V::Str(s) => match s.as_str() {
            Some(t) => String::from(t),
            None => raise(&format!(
                "{surface} is not valid UTF-8, and this library never rewrites a \
                 string: the bytes are {}",
                hex(s.as_bytes())
            )),
        },
        _ => String::new(),
    }
}

/// Lowercase hex, for a refusal that has to print bytes.
fn hex(b: &[u8]) -> String {
    const DIGITS: &[u8; 16] = b"0123456789abcdef";
    let mut out = String::new();
    for x in b {
        out.push(DIGITS[(x >> 4) as usize] as char);
        out.push(DIGITS[(x & 0x0f) as usize] as char);
    }
    out
}

/// [`text`]'s refusal and the hex it prints, which no fixture can reach
/// through the real shim: the surfaces it guards are constrained to ASCII by
/// the engine, so the only way to exercise it is to call it. Off-target
/// [`raise`] panics with the message, which is what makes that possible.
#[cfg(all(test, not(target_family = "wasm")))]
mod refusal_tests {
    use super::*;

    #[test]
    fn text_passes_utf8_through_unchanged() {
        assert_eq!(text("a mod name", &str_("space-age")), "space-age");
        // A non-string is the absent case, not a refusal: an unset mod_name is
        // nil and answers with the empty string.
        assert_eq!(text("the mod's own name", &V::Nil), "");
    }

    // The WHOLE message, because what this guards is that the bytes are named
    // rather than rewritten, and a substring match on the first clause would
    // pass against a message that then printed a replacement character.
    #[test]
    #[should_panic(
        expected = "a mod name is not valid UTF-8, and this library never rewrites a string: the bytes are ff00fe"
    )]
    fn text_refuses_rather_than_rewriting() {
        text("a mod name", &bytes_(&[0xff, 0x00, 0xfe]));
    }

    #[test]
    fn hex_is_lowercase_and_two_digits_per_byte() {
        assert_eq!(hex(&[0x00, 0x0f, 0xa5, 0xff]), "000fa5ff");
        assert_eq!(hex(&[]), "");
    }
}

// ---------------------------------------------------------------------------
// The wire.
// ---------------------------------------------------------------------------

const DYN_W: usize = 16;
const DYN_PW: usize = 32;

/// The three env reads are cached, because they cannot change during a stage
/// and each one crosses a whole dictionary. Nothing else here caches:
/// `data.raw` is mutable by construction and a cached read of it would be a lie
/// the moment the guest's own `set` landed.
///
/// `static mut` rather than a lock: wasm without the threads proposal has one
/// thread, so there is no second accessor to race with -- the same shape every
/// other guest in this workspace uses for its state.
static mut ENV_CACHE: [Option<V>; 5] = [None, None, None, None, None];

fn env_value(which: u32) -> V {
    let idx = (which - 1) as usize;
    unsafe {
        let slot_ptr = &raw mut ENV_CACHE[idx];
        if let Some(v) = (*slot_ptr).as_ref() {
            return v.clone();
        }
        let ret = slot();
        fkd_env(which, ret);
        let v = read_dyn_at(ret);
        *slot_ptr = Some(v.clone());
        v
    }
}

/// Turns a path into one tier-2 array.
fn encode_path(path: &[P]) -> u32 {
    let vs: Vec<V> = path
        .iter()
        .map(|p| match p {
            P::S(s) => V::Str(LuaStr::from(*s)),
            P::B(b) => V::Str(LuaStr::from(*b)),
            P::N(n) => V::Num(*n),
        })
        .collect();
    let p = slot();
    write_dyn_at(p, &V::Arr(vs));
    p
}

/// Hands out one 16-byte tier-2 slot.
///
/// A FRESH ALLOCATION PER CALL RATHER THAN A REUSED STATIC BUFFER: a `get`
/// whose result the guest still holds must not have its slot written over by
/// the next `set`, and the stage runs once and dies, so there is nothing for
/// the allocation to accumulate into.
fn slot() -> u32 {
    let mut b = alloc::vec![0u8; DYN_W];
    let p = b.as_mut_ptr() as u32;
    core::mem::forget(b);
    p
}

fn rd_u32(d: &[u8], at: usize) -> u32 {
    u32::from_le_bytes(d[at..at + 4].try_into().unwrap())
}

fn wr_u32(d: &mut [u8], at: usize, v: u32) {
    d[at..at + 4].copy_from_slice(&v.to_le_bytes());
}

fn read_dyn_at(p: u32) -> V {
    let d = unsafe { core::slice::from_raw_parts(p as *const u8, DYN_W) };
    read_dyn(d)
}

fn write_dyn_at(p: u32, v: &V) {
    let d = unsafe { core::slice::from_raw_parts_mut(p as *mut u8, DYN_W) };
    write_dyn(d, v)
}

fn read_dyn(d: &[u8]) -> V {
    match rd_u32(d, 0) {
        1 => V::Bool(d[8] != 0),
        2 => V::Num(f64::from_le_bytes(d[8..16].try_into().unwrap())),
        3 => {
            let ptr = rd_u32(d, 8) as usize;
            let n = rd_u32(d, 12) as usize;
            if n == 0 {
                return V::Str(LuaStr::new());
            }
            // BYTE FOR BYTE. This line used to be
            // String::from_utf8_lossy(bytes).into_owned(), which rewrote every
            // byte outside UTF-8 into U+FFFD and changed the length while it
            // did it -- so this half and the Go half disagreed about what one
            // wire said, for any value the engine carries through unaltered.
            let bytes = unsafe { core::slice::from_raw_parts(ptr as *const u8, n) };
            V::Str(LuaStr::from(bytes))
        }
        // A data stage never produces one, and there is nothing a guest could
        // do with it: the handle table is the control stage's.
        4 => V::Nil,
        5 => {
            let base = rd_u32(d, 8) as usize;
            let n = rd_u32(d, 12) as usize;
            let mut out = Vec::with_capacity(n);
            for i in 0..n {
                let e =
                    unsafe { core::slice::from_raw_parts((base + i * DYN_W) as *const u8, DYN_W) };
                out.push(read_dyn(e));
            }
            V::Arr(out)
        }
        6 => {
            let base = rd_u32(d, 8) as usize;
            let n = rd_u32(d, 12) as usize;
            let mut out = Vec::with_capacity(n);
            for i in 0..n {
                let k =
                    unsafe { core::slice::from_raw_parts((base + i * DYN_PW) as *const u8, DYN_W) };
                let v = unsafe {
                    core::slice::from_raw_parts((base + i * DYN_PW + DYN_W) as *const u8, DYN_W)
                };
                out.push((read_dyn(k), read_dyn(v)));
            }
            V::Map(out)
        }
        _ => V::Nil,
    }
}

fn write_dyn(d: &mut [u8], v: &V) {
    for b in d[..DYN_W].iter_mut() {
        *b = 0;
    }
    match v {
        V::Nil => wr_u32(d, 0, 0),
        V::Bool(b) => {
            wr_u32(d, 0, 1);
            d[8] = u8::from(*b);
        }
        V::Num(n) => {
            wr_u32(d, 0, 2);
            d[8..16].copy_from_slice(&n.to_le_bytes());
        }
        V::Str(s) => {
            wr_u32(d, 0, 3);
            // The bytes' own storage, unaltered: the host copies them out
            // before the call returns and the value is borrowed for that whole
            // time.
            wr_u32(d, 8, s.as_bytes().as_ptr() as u32);
            wr_u32(d, 12, s.as_bytes().len() as u32);
        }
        V::Obj(h) => {
            wr_u32(d, 0, 4);
            wr_u32(d, 8, *h);
        }
        V::Arr(items) => {
            wr_u32(d, 0, 5);
            let p = galloc((items.len() * DYN_W) as u32);
            for (i, e) in items.iter().enumerate() {
                let s = unsafe {
                    core::slice::from_raw_parts_mut((p as usize + i * DYN_W) as *mut u8, DYN_W)
                };
                write_dyn(s, e);
            }
            wr_u32(d, 8, p);
            wr_u32(d, 12, items.len() as u32);
        }
        V::Map(pairs) => {
            wr_u32(d, 0, 6);
            // SORTED ON THE WAY OUT TOO, so that what a guest sends is a
            // function of what it meant rather than of the order it happened to
            // build it in. The host reads a map back the same way, and two
            // guests that assembled the same prototype differently produce the
            // same bytes.
            let mut kvs = pairs.clone();
            sort_pairs(&mut kvs);
            let p = galloc((kvs.len() * DYN_PW) as u32);
            for (i, (k, val)) in kvs.iter().enumerate() {
                let ks = unsafe {
                    core::slice::from_raw_parts_mut((p as usize + i * DYN_PW) as *mut u8, DYN_W)
                };
                write_dyn(ks, k);
                let vs = unsafe {
                    core::slice::from_raw_parts_mut(
                        (p as usize + i * DYN_PW + DYN_W) as *mut u8,
                        DYN_W,
                    )
                };
                write_dyn(vs, val);
            }
            wr_u32(d, 8, p);
            wr_u32(d, 12, kvs.len() as u32);
        }
    }
}

/// A stable insertion sort by key.
///
/// HAND-WRITTEN RATHER THAN `sort_by`, so that the two languages sort with
/// literally the same algorithm as well as the same comparison -- a stable sort
/// and an unstable one differ on equal keys, and a prototype with two identical
/// keys is exactly the sort of thing a mirror test should catch rather than
/// paper over.
fn sort_pairs(kvs: &mut [(V, V)]) {
    for i in 1..kvs.len() {
        let mut j = i;
        while j > 0 && key_less(&kvs[j].0, &kvs[j - 1].0) {
            kvs.swap(j, j - 1);
            j -= 1;
        }
    }
}

/// The same total order `fk_data.lua` sorts with: numbers before strings, each
/// in their own natural order. Stated twice, in two languages, because a wire
/// both sides sort has to agree about what sorted means.
fn key_less(a: &V, b: &V) -> bool {
    let (ra, rb) = (key_rank(a), key_rank(b));
    if ra != rb {
        return ra < rb;
    }
    match (a, b) {
        (V::Num(x), V::Num(y)) => x < y,
        // BYTE-LEXICOGRAPHIC, which is what LuaStr's Ord is, what Go's string
        // comparison is, and what Lua's own `<` over strings is in the C
        // locale. All three have to agree or the wire order does not.
        (V::Str(x), V::Str(y)) => x < y,
        _ => false,
    }
}

fn key_rank(v: &V) -> u8 {
    match v {
        V::Num(_) => 1,
        V::Str(_) => 2,
        _ => 3,
    }
}

/// The guest's own allocator, reached the way `fkapi` reaches it: through the
/// `fk_alloc` export `fk` defines, so there is exactly one allocator in the
/// module graph.
#[cfg(target_family = "wasm")]
fn galloc(n: u32) -> u32 {
    if n == 0 {
        return 0;
    }
    fk::fk_alloc(n)
}

/// Off-target there is no guest heap and no 32-bit address space to put one in,
/// which is why a host build of this crate tests the value model and never the
/// wire. Reaching here means a test called an operation that crosses.
#[cfg(not(target_family = "wasm"))]
fn galloc(_n: u32) -> u32 {
    panic!("fkdata: the tier-2 wire is guest memory and does not exist off-target")
}
