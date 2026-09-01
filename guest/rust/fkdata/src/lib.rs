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

#![no_std]

extern crate alloc;

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
pub fn log(s: &str) {
    fk::log(s)
}

/// Stops the load with THIS GUEST'S OWN message, exactly as a host-detected
/// failure does: the stage name is prefixed, the error unwinds the whole
/// stage, and the call never returns.
///
/// This is what a validating guest uses instead of panicking. A panic
/// surfaces in the player's game as `fklua trap: unreachable`, with whatever
/// diagnostic was built surviving only as a log line above it; `raise` puts
/// the message where the player looks. A cycle detector naming its path, a
/// presence check naming the missing prototype, a refused configuration
/// naming the setting: this is their exit.
pub fn raise(msg: &str) -> ! {
    unsafe { fkd_raise(msg.as_ptr() as u32, msg.len() as u32) };
    // The host raise unwinds the Lua stage through this call, so execution
    // never resumes here. Under a stand-in that failed to raise, stopping
    // hard beats running on past a load that should have been refused.
    unreachable!("fkdata: raise returned")
}

// ---------------------------------------------------------------------------
// The value model.
// ---------------------------------------------------------------------------

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
    Str(String),
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

/// A string.
pub fn str_(s: &str) -> V {
    V::Str(String::from(s))
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
            .map(|(k, v)| (V::Str(String::from(*k)), v.clone()))
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

    /// Reads a string value, or "".
    pub fn string(&self) -> &str {
        match self {
            V::Str(s) => s.as_str(),
            _ => "",
        }
    }

    /// Reads a boolean value, or false.
    pub fn boolean(&self) -> bool {
        matches!(self, V::Bool(true))
    }

    /// Looks one key up in a map. `None` for anything that is not a map, and
    /// for a key the map does not have.
    pub fn at(&self, key: &str) -> Option<&V> {
        match self {
            V::Map(pairs) => pairs
                .iter()
                .find(|(k, _)| k.string() == key && matches!(k, V::Str(_)))
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
#[derive(Clone, Debug)]
pub enum P<'a> {
    S(&'a str),
    N(f64),
}

impl<'a> From<&'a str> for P<'a> {
    fn from(s: &'a str) -> Self {
        P::S(s)
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
pub fn keys(path: &[P]) -> Vec<String> {
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
pub fn mods() -> Vec<(String, String)> {
    match env_value(1) {
        V::Map(pairs) => pairs
            .iter()
            .map(|(k, v)| (String::from(k.string()), String::from(v.string())))
            .collect(),
        _ => Vec::new(),
    }
}

/// One mod's version, and whether it is installed at all.
pub fn mod_version(name: &str) -> Option<String> {
    env_value(1).at(name).map(|v| String::from(v.string()))
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
            .map(|(k, v)| (String::from(k.string()), v.boolean()))
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
    String::from(env_value(4).string())
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
                V::Str(s) => Some(s.clone()),
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
                if items.iter().any(|d| d.string() == derived) {
                    return Some(String::from(k.string()));
                }
            }
        }
    }
    None
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
            P::S(s) => V::Str(String::from(*s)),
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
                return V::Str(String::new());
            }
            let bytes = unsafe { core::slice::from_raw_parts(ptr as *const u8, n) };
            V::Str(String::from_utf8_lossy(bytes).into_owned())
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
            wr_u32(d, 8, s.as_ptr() as u32);
            wr_u32(d, 12, s.len() as u32);
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
fn galloc(n: u32) -> u32 {
    if n == 0 {
        return 0;
    }
    fk::fk_alloc(n)
}
