package factorio

// rustRuntime is the hand-written part of the generated Rust module.
//
// The tier-2 Value is the piece worth reading: an ENUM, where Go got a struct
// with six fields of which five are always dead. Same wire, same tags, same
// offsets -- the difference is only what the language can say.
const rustRuntime = `
// The ABI names these fk.call, fk.subscribe and fk.define. #[link_name] is what
// keeps the Rust identifier readable while the IMPORT keeps the name
// control.lua binds -- without it the module asks for fk.fk_call and refuses to
// instantiate.
#[link(wasm_import_module = "fk")]
extern "C" {
    #[link_name = "call"]
    fn fk_call(handle: u32, member: u32, argp: u32, retp: u32) -> u32;
    #[link_name = "subscribe"]
    fn fk_subscribe(event: u32, filterp: u32, skip: u32) -> u32;
    #[link_name = "define"]
    fn fk_define(id: u32) -> u32;
    // RETAIN AND RELEASE, which this backend simply did not declare until
    // 2026-08-03 -- the Go runtime has had them since M6 and the Rust one was
    // written without them, so a Rust guest could not keep a handle past its
    // event at all. Flagged by the B1a parity round as the one asymmetry it
    // found and did not close. See Object::retain below for what they mean.
    #[link_name = "retain"]
    fn fk_retain(handle: u32) -> u32;
    #[link_name = "release"]
    fn fk_release(handle: u32) -> u32;
    // THE CALLBACK SEAM. A Lua function cannot cross this boundary in either
    // direction, so the host synthesises a closure and dispatches into this
    // guest by an id the guest chose. See add_command below.
    #[link_name = "register"]
    fn fk_register(kind: u32, descp: u32) -> u32;
    #[link_name = "remote_call"]
    fn fk_remote_call(callp: u32, retp: u32) -> u32;
}

extern "C" {
    fn fk_alloc(n: u32) -> u32;
}

/// Safe wrapper over the guest's allocator export.
///
/// Allocating is not itself unsafe -- it returns an address or zero. Only the
/// extern declaration makes it so, and pushing the keyword into every generated
/// call site would spend the keyword where it means nothing and train a reader
/// to skip it where it means everything.
#[inline]
fn galloc(n: u32) -> u32 {
    unsafe { fk_alloc(n) }
}

/// A handle into the host's table. Opaque by design: the number means nothing
/// to a guest beyond identifying an object.
/// Ord so a handle can key a BTreeMap -- 5 members return a dictionary keyed by
/// one. The order is the handle number's, which is arbitrary but total, and a
/// map only needs it to be consistent.
#[derive(Copy, Clone, PartialEq, Eq, PartialOrd, Ord, Hash, Debug, Default)]
pub struct Object(pub u32);

impl Object {
    /// Makes a handle survive past the current event. Release it when done.
    ///
    /// Handles the host produces are TRANSIENT: they stop working when the
    /// event that produced them returns. That is the default that makes the
    /// dominant leak shape free -- take a handle, use it, drop it, and nothing
    /// accumulates -- and this is the opt-out for one that has to outlive the
    /// event, which is what any guest keeping state across ticks needs.
    #[inline]
    pub fn retain(self) -> Object {
        Object(unsafe { fk_retain(self.0) })
    }

    /// Frees a retained handle. Releasing a transient one is harmless.
    #[inline]
    pub fn release(self) {
        unsafe {
            fk_release(self.0);
        }
    }

    /// Reports a handle that is not the null one. It does not ask the game
    /// whether the object behind it still exists -- a call does that, and
    /// reports Status::INVALID.
    #[inline]
    pub fn is_valid(self) -> bool {
        self.0 != 0
    }
}

/// A host-call status. Never a panic: there are no coroutines, so an error
/// crossing back into wasm could not unwind the frame it came from.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub struct Status(pub u32);

impl Status {
    pub const BAD_HANDLE: u32 = 1;
    pub const INVALID: u32 = 2;
    pub const NO_MEMBER: u32 = 3;
    pub const BAD_ARGS: u32 = 4;
    pub const CALL_FAILED: u32 = 5;
    pub const NO_SPACE: u32 = 6;

    pub fn as_str(self) -> &'static str {
        match self.0 {
            1 => "not a live handle",
            2 => "the object is no longer valid",
            3 => "no such member on this Factorio version",
            4 => "bad arguments",
            5 => "the Factorio API raised",
            6 => "out of space",
            _ => "unknown status",
        }
    }
}

/// Subscribes to an event. The ids are the EVENT_* constants below.
///
/// #[inline(always)] for the same reason its filtered sibling carries one --
/// see there. This one is small enough that rustc has always inlined it under
/// LTO, which is exactly why nothing noticed the attribute was the load-bearing
/// part rather than the size.
#[inline(always)]
pub fn subscribe(event: u32) -> Status {
    Status(unsafe { fk_subscribe(event, 0, 0) })
}

/// Subscribes and declares the payload fields this guest never reads, so the
/// host stops encoding them.
///
/// The encode is EAGER and complete: every field of an event is marshalled into
/// the scratch buffer before the handler is entered. That is right for a flat
/// payload -- the mean event has 4.8 fields and a host call per field would
/// cost more -- and wrong for the few that carry a container.
/// on_undo_applied's "actions" field is an array of tier-2 values, so an undo
/// step's whole BlueprintEntity list is deep-copied before a handler wanting
/// one u32 runs. Measured through the real dispatch protocol: 200 actions,
/// 7.49 ms -> 2.7 us.
///
///     subscribe_masked(EVENT_ON_UNDO_APPLIED, SKIP_ON_UNDO_APPLIED_ACTIONS);
///
/// OR the SKIP_* constants together. Only OPTIONAL and CONTAINER fields have
/// one, and that restriction is the safety property: a masked optional reads as
/// ABSENT and a masked container as EMPTY, both of which every generated
/// decoder already produces, so a mask that is wrong costs a value you did not
/// get rather than a zero you cannot tell from a real one. A bit naming
/// anything else is logged at subscribe time and ignored.
///
/// THE LAYOUT DOES NOT MOVE. Fields keep the offsets they were compiled at;
/// only their contents go away.
///
/// #[inline(always)] for the pruning reason its siblings carry -- see
/// subscribe_filtered.
#[inline(always)]
pub fn subscribe_masked(event: u32, skip: u32) -> Status {
    Status(unsafe { fk_subscribe(event, 0, skip) })
}

/// Subscribes with Factorio's own event filters, which the engine applies in
/// C++ BEFORE the handler runs.
///
/// Without them a guest that cares about one prototype is entered for every
/// build and mine event on the map and pays a dispatch, a host call and a
/// string crossing to read entity.name and reject it. With them it is not
/// entered at all.
///
///     subscribe_filtered(EVENT_ON_PLAYER_MINED_ENTITY,
///                        &name_filter(&["my-machine"]));
///
/// Terms are OR-ed, which is Factorio's default. Filters are decoded once, at
/// subscribe time, so none of this is a per-event cost. Two subscriptions to
/// the same event share a registration and their filters are UNION-ed; an
/// unfiltered one widens the pair, which is the only merge that cannot silently
/// stop delivering an event somebody asked for.
///
/// # The filter grammar, and how to write a term this crate has no helper for
///
/// A term is a MAP whose "filter" key names the condition and whose other keys
/// are that condition's own parameters. name_filter and type_filter build the
/// two commonest; everything else is an ordinary Value::Map and this takes them
/// as they come. The three shapes side by side, all equivalent to the Lua
/// tables the engine documents:
///
///     name_filter(&["iron-chest"])        {filter="name", name="iron-chest"}
///     type_filter(&["tree"])              {filter="type", type="tree"}
///     Value::Map(alloc::vec![             {filter="type", type="tree", invert=true}
///         (Value::Str("filter".into()), Value::Str("type".into())),
///         (Value::Str("type".into()),   Value::Str("tree".into())),
///         (Value::Str("invert".into()), Value::Bool(true)),
///     ])
///
/// WHICH conditions an event accepts is per event and is in the API description
/// this crate was generated from, as the Lua<Event>EventFilter concepts --
/// LuaPlayerMinedEntityEventFilter, LuaEntityDiedEventFilter and 29 more. Read
/// them out of api/<version>/runtime-api.json, or online at
/// lua-api.factorio.com/<version>/concepts/Lua<Event>EventFilter.html. Most
/// entity events take "name", "type", "ghost_name" and "ghost_type" plus a
/// handful of category conditions ("rail", "turret",
/// "transport-belt-connectable", ...) that carry no parameter of their own -- a
/// one-pair Value::Map.
///
/// Every term also takes the optional "mode" ("or", the default, or "and" --
/// which binds tighter) and "invert". A filter the event does not accept is
/// refused by the ENGINE at subscribe time, not here, so it surfaces as a Lua
/// error in the log naming the term.
///
/// #[inline(always)] IS LOAD-BEARING AND IT IS ABOUT MOD SIZE, NOT SPEED.
/// fklua mod ships only the event descriptors it can PROVE a guest subscribes
/// to, by scanning the wasm for an i32.const reaching fk.subscribe's first
/// operand, and it is all-or-nothing: one id it cannot prove and all 224 ship.
/// This function is several times the size of subscribe() -- an early return,
/// an AllocMark, a galloc and a write_dyn -- so without the attribute, whether
/// the id reaches the import as a constant is rustc's cost heuristic's
/// decision, taken per call site. Measured on this repo's own examples/api:
/// ONE filtered subscription inlines and EIGHT do not, so a guest crosses the
/// line by GROWING rather than by changing anything.
///
/// Reported from the field before it was reproduced here (a downstream Rust
/// port with eight filtered subscriptions): "all 218 events -- an
/// event id was not a compile-time constant", and 991,040 bytes of Lua against
/// 906,393 for the same mod with the filters taken out. 85 KB, parsed by the
/// game on every load, in every save. The port kept the filters and paid it,
/// because filters are BEHAVIOUR and the table is only size -- which is the
/// trade this attribute deletes.
///
/// Gated by TestTheEventIdSurvivesTheGeneratedRustSubscribeWrapper.
#[inline(always)]
pub fn subscribe_filtered(event: u32, filters: &[Value]) -> Status {
    subscribe_filtered_masked(event, 0, filters)
}

/// subscribe_filtered and subscribe_masked at once: the engine drops the events
/// this guest does not want, and the host does not encode the fields it will
/// not read from the ones that survive.
#[inline(always)]
pub fn subscribe_filtered_masked(event: u32, skip: u32, filters: &[Value]) -> Status {
    if filters.is_empty() {
        return subscribe_masked(event, skip);
    }
    let _m = AllocMark::new();
    let p = galloc(DYN_W as u32);
    let d = unsafe { core::slice::from_raw_parts_mut(p as *mut u8, DYN_W) };
    write_dyn(d, &Value::Array(filters.to_vec()));
    Status(unsafe { fk_subscribe(event, p, skip) })
}

/// The descriptor kinds fk.register takes, mirroring fk_mod.lua's REG_COMMAND
/// and REG_INTERFACE.
const REG_COMMAND: u32 = 1;
const REG_INTERFACE: u32 = 2;

/// Declares a console command, handled by this guest's fk_on_call export.
///
/// THE HANDLER DOES NOT CROSS THE BOUNDARY, and cannot: a wasm guest has no
/// callable Lua value. What crosses is id, an integer this guest chooses; the
/// host synthesises a Lua closure that captures it, hands THAT to
/// commands.add_command, and dispatches back in through fk_on_call when the
/// command is typed.
///
/// const CMD_HELLO: u32 = 1;
///
/// #[no_mangle]
/// pub extern "C" fn _initialize() {
///     fkapi::add_command(CMD_HELLO, "hello", &Value::Str("says hello".into()));
/// }
///
/// #[no_mangle]
/// pub extern "C" fn fk_on_call(id: u32, argp: u32, retp: u32) -> u32 {
///     if id == CMD_HELLO { /* fkapi::read_dyn_at(argp) is [CustomCommandData] */ }
///     let _ = retp;
///     0
/// }
///
/// CALL IT FROM _initialize, for a reason stronger than the one subscribe
/// gives. A command registration is not saved: Factorio re-executes control.lua
/// on every load, so it has to be made on every load, and _initialize is the
/// only place that happens by construction.
///
/// help is a LocalisedString -- either a Str or an Array of a key and its
/// parameters.
pub fn add_command(id: u32, name: &str, help: &Value) -> Status {
    let _m = AllocMark::new();
    let p = galloc(DYN_W as u32);
    let d = unsafe { core::slice::from_raw_parts_mut(p as *mut u8, DYN_W) };
    write_dyn(
        d,
        &Value::Map(alloc::vec![
            (Value::Str("name".into()), Value::Str(name.into())),
            (Value::Str("help".into()), help.clone()),
            (Value::Str("id".into()), Value::Number(id as f64)),
        ]),
    );
    Status(unsafe { fk_register(REG_COMMAND, p) })
}

/// Declares a remote interface whose methods are handled by this guest's
/// fk_on_call export.
///
/// methods maps a method name to the id fk_on_call will be given. Everything
/// add_command says about ids, closures and _initialize applies unchanged; the
/// one difference is that a remote method's RESULT is used, so fk_on_call
/// should write one through write_dyn_at(retp, ..).
///
/// The slice is taken in the order given and encoded in it, so a caller that
/// wants a stable guest heap across clients passes a stable order -- the same
/// property the pair-slice dictionary returns have.
pub fn add_interface(name: &str, methods: &[(&str, u32)]) -> Status {
    if methods.is_empty() {
        return Status(4); // ERR_BAD_ARGS
    }
    let _m = AllocMark::new();
    let mut kv: Vec<(Value, Value)> = Vec::with_capacity(methods.len());
    for (n, id) in methods {
        kv.push((Value::Str((*n).into()), Value::Number(*id as f64)));
    }
    let p = galloc(DYN_W as u32);
    let d = unsafe { core::slice::from_raw_parts_mut(p as *mut u8, DYN_W) };
    write_dyn(
        d,
        &Value::Map(alloc::vec![
            (Value::Str("name".into()), Value::Str(name.into())),
            (Value::Str("methods".into()), Value::Map(kv)),
        ]),
    );
    Status(unsafe { fk_register(REG_INTERFACE, p) })
}

/// remote.call: the outbound half of mod-to-mod interop.
///
/// The member itself is unbindable -- it is the API's one variadic method, and
/// the ABI's argument block has a fixed shape decided at generate time -- so the
/// arguments are packed into one tier-2 array instead. There is no arity
/// ceiling.
///
/// A missing interface or method is a failed Status rather than a trap, because
/// the other mod not being installed is an ordinary thing to have an opinion
/// about.
pub fn remote_call(iface: &str, method: &str, args: &[Value]) -> Result<Value, Status> {
    let _m = AllocMark::new();
    let p = galloc((DYN_W * 2) as u32);
    let d = unsafe { core::slice::from_raw_parts_mut(p as *mut u8, DYN_W) };
    write_dyn(
        d,
        &Value::Array(alloc::vec![
            Value::Str(iface.into()),
            Value::Str(method.into()),
            Value::Array(args.to_vec()),
        ]),
    );
    let st = unsafe { fk_remote_call(p, p + DYN_W as u32) };
    if st != 0 {
        return Err(Status(st));
    }
    let r = unsafe { core::slice::from_raw_parts((p as usize + DYN_W) as *const u8, DYN_W) };
    Ok(read_dyn(r))
}

/// Decodes the tier-2 value at a pointer the host handed this guest --
/// fk_on_call's argp, and nothing else today.
pub fn read_dyn_at(p: u32) -> Value {
    let d = unsafe { core::slice::from_raw_parts(p as *const u8, DYN_W) };
    read_dyn(d)
}

/// Encodes a tier-2 value into a slot the host handed this guest --
/// fk_on_call's retp, and nothing else today. A guest that writes nothing
/// leaves the slot as the host cleared it, which reads back as nil.
pub fn write_dyn_at(p: u32, v: &Value) {
    let d = unsafe { core::slice::from_raw_parts_mut(p as *mut u8, DYN_W) };
    write_dyn(d, v);
}

/// Builds the commonest event filter there is: only these prototype names.
pub fn name_filter(names: &[&str]) -> Vec<Value> {
    names
        .iter()
        .map(|n| {
            Value::Map(alloc::vec![
                (
                    Value::Str(LuaStr::from("filter")),
                    Value::Str(LuaStr::from("name"))
                ),
                (
                    Value::Str(LuaStr::from("name")),
                    Value::Str(LuaStr::from(*n))
                ),
            ])
        })
        .collect()
}

/// name_filter's twin over prototype TYPE: {filter="type", type="tree"} rather
/// than {filter="name", name="tree-01"}. One term catches a whole family, which
/// is what a guest that cares about "any tree" or "any assembling-machine"
/// wants -- names are per prototype and there are hundreds.
///
/// Terms are OR-ed with everything else in the same subscribe_filtered call, so
/// mixing the two is a concat of the two Vecs.
pub fn type_filter(types: &[&str]) -> Vec<Value> {
    types
        .iter()
        .map(|t| {
            Value::Map(alloc::vec![
                (
                    Value::Str(LuaStr::from("filter")),
                    Value::Str(LuaStr::from("type"))
                ),
                (
                    Value::Str(LuaStr::from("type")),
                    Value::Str(LuaStr::from(*t))
                ),
            ])
        })
        .collect()
}

#[inline]
fn rd_u32(d: &[u8], at: usize) -> u32 {
    u32::from_le_bytes(d[at..at + 4].try_into().unwrap())
}

#[inline]
fn wr_u32(d: &mut [u8], at: usize, v: u32) {
    d[at..at + 4].copy_from_slice(&v.to_le_bytes());
}

/// A LUA STRING, WHICH IS A BYTE STRING AND NOT UTF-8.
///
/// This is the one place the Go rendering was straightforwardly RIGHT and this
/// one was wrong, and it took four milestones and a Rust mod to notice. A Lua
/// string is an arbitrary byte sequence -- string.char(0xFF) is a Lua string,
/// and so is a UDP datagram, a helpers.write_file payload and anything a mod
/// put in a tags entry. Go's string is exactly that, so getStr is
/// string(b) and copies the bytes. Rust's String is NOT: it carries a
/// validity invariant that safe code relies on, so the binding used
/// String::from_utf8_lossy(..).into_owned() -- which turns every byte outside
/// UTF-8 into U+FFFD, SILENTLY, and changes the length while it does it. Two
/// generated readers over one wire disagreed about what came back.
///
/// So the byte string gets a type. LuaStr owns a Vec<u8>, is byte-exact in
/// both directions, and keeps the text case one clearly named call away:
/// [LuaStr::as_str] when a caller wants the conversion CHECKED, and
/// [LuaStr::to_string_lossy] -- and Display -- when it wants the old
/// behaviour named out loud. From<&str> means Value::Str("name".into())
/// still reads the way it did.
///
/// from_utf8_unchecked was the cheap alternative and it is not available:
/// handing safe Rust a String whose bytes are not UTF-8 is library UB the
/// moment anything slices, iterates or formats it, and these bytes come from
/// the engine rather than from the guest. (guest/rust/fkipc was already doing
/// exactly that to send a binary frame, which is one more thing this type
/// retires.)
///
/// Ord and Hash so it can key a BTreeMap -- byte-lexicographic, which is the
/// same order String gave, so no dictionary's iteration order moves.
#[derive(Clone, Default, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct LuaStr(Vec<u8>);

impl LuaStr {
    #[inline]
    pub fn new() -> Self {
        LuaStr(Vec::new())
    }

    #[inline]
    pub fn as_bytes(&self) -> &[u8] {
        &self.0
    }

    #[inline]
    pub fn into_bytes(self) -> Vec<u8> {
        self.0
    }

    /// The bytes as text, or None when they are not UTF-8. THE CHECKED
    /// CONVERSION: a caller that needs a &str and cares whether it got one
    /// asks for it here.
    #[inline]
    pub fn as_str(&self) -> Option<&str> {
        core::str::from_utf8(&self.0).ok()
    }

    /// The old get_str behaviour, named. Every byte outside UTF-8 becomes
    /// U+FFFD and the length changes with it, which is fine for logging and
    /// wrong for anything that has to go back out.
    #[inline]
    pub fn to_string_lossy(&self) -> Cow<'_, str> {
        String::from_utf8_lossy(&self.0)
    }

    /// Replaces the contents, REUSING the allocation where the capacity
    /// allows. This is what lets a guest that sends every tick hold one
    /// Value::Str and refill it, instead of allocating a copy per send --
    /// see the note on Value.
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

/// So a BTreeMap keyed by LuaStr can be looked up without building one:
/// tags.get("colour".as_bytes()).
///
/// Borrow<[u8]> and NOT Borrow<str>, which is what a caller would rather write.
/// Borrow is infallible and Ord-consistent by contract, and a LuaStr holding
/// bytes that are not UTF-8 has no &str to hand back -- so the borrowed form
/// has to be the bytes. Ord and Hash agree either way: both are
/// byte-lexicographic over the same bytes.
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
/// out of as_bytes and never out of a formatter.
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

/// Writes a (pointer, length) pair over the bytes' own storage.
///
/// Safe precisely because the host copies them out before the call returns, and
/// the bytes are still borrowed by the caller for that whole time.
///
/// Takes &[u8] rather than &str so one function serves both directions: a
/// string ARGUMENT is a &str (byte-exact for anything a &str can hold) and a
/// string FIELD is a [LuaStr], and .as_bytes() is what both offer.
fn put_str(d: &mut [u8], at: usize, s: &[u8]) {
    wr_u32(d, at, s.as_ptr() as u32);
    wr_u32(d, at + 4, s.len() as u32);
}

/// Copies a (pointer, length) the host wrote, BYTE FOR BYTE.
///
/// One Vec per string, which is the same allocation count the lossy version
/// had -- from_utf8_lossy(..).into_owned() copies too. What changed is that
/// the bytes arrive unaltered and len() is the length the host wrote.
fn get_str(d: &[u8], at: usize) -> LuaStr {
    let p = rd_u32(d, at) as usize;
    let n = rd_u32(d, at + 4) as usize;
    if p == 0 || n == 0 {
        return LuaStr::new();
    }
    let b = unsafe { core::slice::from_raw_parts(p as *const u8, n) };
    LuaStr(b.to_vec())
}

/// Brackets one host call's allocations.
///
/// A call can allocate on both sides and a dynamic value nests arbitrarily, so
/// a tree walk to free it would have to mirror write_dyn exactly and would rot
/// the first time either changed. Rust makes this cheaper than the Go side's
/// explicit mark/release pair: Drop runs it, so a binding cannot forget.
///
/// The guest allocator never reclaims (see guest/rust/fk), so this is a marker
/// rather than a free -- it exists so a future allocator has the shape it needs.
pub struct AllocMark;

impl AllocMark {
    pub fn new() -> Self {
        AllocMark
    }
}

impl Drop for AllocMark {
    fn drop(&mut self) {}
}

/// A tier-2 dynamic value: what the host sends where the API's type is a union,
/// a LocalisedString, or anything else without a fixed layout.
///
/// An enum, not a tagged struct. The Go binding carries six fields and reads
/// one; this carries exactly what the tag says is there.
#[derive(Clone, Debug, Default, PartialEq)]
pub enum Value {
    #[default]
    Nil,
    Bool(bool),
    Number(f64),
    /// A byte string, not a String: see [LuaStr]. Value::Str("x".into())
    /// is unchanged from when this held a String.
    ///
    /// # The one allocation a sender can still avoid
    ///
    /// Every method taking a tier-2 value takes it BY REFERENCE, so a guest
    /// that sends on a schedule builds its Value once and refills the
    /// LuaStr in place with [LuaStr::set] -- no copy per send, which is
    /// what the Go side gets from unsafe.String over its own buffer. Building
    /// a fresh Value::Str per send copies the payload, and under the default
    /// bump allocator nothing takes it back.
    Str(LuaStr),
    Obj(Object),
    Array(Vec<Value>),
    /// A Vec of pairs rather than a map: the host's order is meaningful in a
    /// lockstep game, and a Value key would not be Ord anyway.
    Map(Vec<(Value, Value)>),
}

const DYN_W: usize = 16;
const DYN_PW: usize = 32;

fn read_dyn(d: &[u8]) -> Value {
    match rd_u32(d, 0) {
        1 => Value::Bool(d[8] != 0),
        2 => Value::Number(f64::from_le_bytes(d[8..16].try_into().unwrap())),
        3 => Value::Str(get_str(d, 8)),
        4 => Value::Obj(Object(rd_u32(d, 8))),
        5 => {
            let base = rd_u32(d, 8) as usize;
            let n = rd_u32(d, 12) as usize;
            let mut out = Vec::with_capacity(n);
            for i in 0..n {
                let e = unsafe { core::slice::from_raw_parts((base + i * DYN_W) as *const u8, DYN_W) };
                out.push(read_dyn(e));
            }
            Value::Array(out)
        }
        6 => {
            let base = rd_u32(d, 8) as usize;
            let n = rd_u32(d, 12) as usize;
            let mut out = Vec::with_capacity(n);
            for i in 0..n {
                let k = unsafe { core::slice::from_raw_parts((base + i * DYN_PW) as *const u8, DYN_W) };
                let v = unsafe {
                    core::slice::from_raw_parts((base + i * DYN_PW + DYN_W) as *const u8, DYN_W)
                };
                out.push((read_dyn(k), read_dyn(v)));
            }
            Value::Map(out)
        }
        _ => Value::Nil,
    }
}

fn write_dyn(d: &mut [u8], v: &Value) {
    for b in d[..DYN_W].iter_mut() {
        *b = 0;
    }
    match v {
        Value::Nil => wr_u32(d, 0, 0),
        Value::Bool(b) => {
            wr_u32(d, 0, 1);
            d[8] = if *b { 1 } else { 0 };
        }
        Value::Number(n) => {
            wr_u32(d, 0, 2);
            d[8..16].copy_from_slice(&n.to_le_bytes());
        }
        Value::Str(s) => {
            wr_u32(d, 0, 3);
            put_str(d, 8, s.as_bytes());
        }
        Value::Obj(o) => {
            wr_u32(d, 0, 4);
            wr_u32(d, 8, o.0);
        }
        Value::Array(items) => {
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
        Value::Map(pairs) => {
            wr_u32(d, 0, 6);
            let p = galloc((pairs.len() * DYN_PW) as u32);
            for (i, (k, val)) in pairs.iter().enumerate() {
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
            wr_u32(d, 12, pairs.len() as u32);
        }
    }
}
`
