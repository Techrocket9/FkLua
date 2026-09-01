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
    // The same dispatch over a TYPED argument block: same handle, same member
    // id, same return block, and an argument block laid out as a tier-1 struct
    // plus one optional tier-2 slot instead of one tier-2 map. Only a member
    // whose parameter table is a discriminated union has one -- see the
    // <name>_typed bindings.
    #[link_name = "call_typed"]
    fn fk_call_typed(handle: u32, member: u32, argp: u32, retp: u32) -> u32;
    // ONE ATTRIBUTE OFF N HANDLES IN ONE CROSSING, which is what every
    // <class>_<name>_bulk free function in this module is.
    //
    // The member is the ORDINARY getter's id -- there is no bulk member and no
    // new id anywhere, which is why a mod that reads only in bulk still prunes
    // to the one member it named. handlep points at count u32 handles, which is
    // exactly what a &[LuaEntity] already is; dstp at count copies of that
    // getter's own return block; and retp at four bytes the host writes the
    // number of elements it actually read into.
    //
    // AN ELEMENT THAT CANNOT BE READ IS SKIPPED, NOT FATAL. A dead handle, an
    // object whose valid went false, or a read the engine raised on writes that
    // element as the ZERO value -- never leaving the previous crossing's value
    // there, which would be the plausible wrong answer -- and does not count
    // toward the return. So a count below the slice length says something was
    // missed, and for an attribute the description marks OPTIONAL the presence
    // byte on each element says which. For a mandatory one, a zero that was
    // skipped and a zero that was read are the same bytes; that is the honest
    // limit of a flat destination.
    #[link_name = "bulk_get"]
    fn fk_bulk_get(member: u32, handlep: u32, count: u32, dstp: u32, retp: u32) -> u32;
    #[link_name = "subscribe"]
    fn fk_subscribe(event: u32, filterp: u32, skip: u32, namep: u32, namelen: u32) -> u32;
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

/// Where fk_bulk_get writes the number of elements it read.
///
/// A STATIC RATHER THAN A STACK SLOT, which is the Go side's own reasoning one
/// language over: a four-byte block through the arena would be a bracket per
/// call for one number, and a stack address handed to the host is one more thing
/// to reason about. It is read on the line after the call returns and a host
/// call cannot re-enter this guest between the two, so A BULK READ ALLOCATES
/// NOTHING AT ALL.
///
/// A wasm module is single-threaded by construction here -- Factorio runs the
/// mod on one Lua state -- so the static mut is sound for the same reason the
/// arena's own cursor is.
static mut BULK_READ: u32 = 0;

/// A HANDLE IS FOUR BYTES, AND EVERY GENERATED CLASS TYPE IS ONE HANDLE WIDE.
///
/// That is what lets a bulk read take a &[LuaEntity] and hand the host
/// objs.as_ptr() as an array of u32 handles, with no copy and no conversion: the
/// slice the search already wrote IS the handle array. #[repr(transparent)] on
/// every handle newtype is what makes it true rather than merely observed, and
/// this asserts the bottom of the chain.
const _: () = assert!(core::mem::size_of::<Object>() == 4);

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
/// #[repr(transparent)] is load-bearing rather than tidy: a &[Object] is handed
/// to the host as an array of u32 handles by a bulk read, and repr(Rust) leaves
/// a newtype's layout unspecified. See BULK_READ above.
#[derive(Copy, Clone, PartialEq, Eq, PartialOrd, Ord, Hash, Debug, Default)]
#[repr(transparent)]
pub struct Object(pub u32);

impl Object {
    /// The lowest handle the host ALLOCATES. Below it sit the nine fixed
    /// globals, which are bound at load and are neither allocated nor freed.
    pub const FIRST_DYNAMIC: u32 = 10;

    /// The split point between the persistent space and the transient one. A
    /// handle at or above this is discarded when the OUTERMOST dispatch
    /// returns.
    ///
    /// Both numbers are fk_abi.lua's, and a host-side test reads them out of
    /// that file rather than trusting this copy of them.
    pub const TRANSIENT_BASE: u32 = 0x4000_0000;

    /// Makes a handle survive past the current event. Release it when done.
    ///
    /// Handles the host produces are TRANSIENT: they stop working when the
    /// event that produced them returns. That is the default that makes the
    /// dominant leak shape free -- take a handle, use it, drop it, and nothing
    /// accumulates -- and this is the opt-out for one that has to outlive the
    /// event, which is what any guest keeping state across ticks needs.
    ///
    /// WHAT IT DOES DEPENDS ON THE SPACE, and that difference is what the guard
    /// below is built on. Retaining a TRANSIENT handle mints a NEW persistent
    /// slot, every time: two retains of one transient handle are two slots onto
    /// one object, each owed its own release. Retaining a handle that is ALREADY
    /// PERSISTENT AND STILL OCCUPIED, or a global the game has bound, hands the
    /// same number straight back without allocating -- so a second retain there
    /// buys no ownership, and a second release of it frees the first owner's
    /// slot.
    ///
    /// A RETAIN CAN ALSO FAIL, IN ANY OF THE THREE SPACES, and the two clauses
    /// above are the cases where it does not. It hands back the NULL handle
    /// with Status::BAD_HANDLE for a persistent number whose slot is NOT
    /// occupied -- one already released, or one built with Object(n) and never
    /// retained -- for a global the game has not bound, and for a transient
    /// number this dispatch never handed out; and Status::NO_SPACE when the
    /// persistent space is exhausted. Each of those is measured against
    /// fk_abi.lua's M.retain. This raw form hands back a bare Object and no
    /// status, so CHECK is_valid() ON WHAT COMES BACK: a guest that does not is
    /// left holding a handle over nothing, and its release is a discarded
    /// Status::BAD_HANDLE that says so to nobody.
    ///
    /// Prefer Object::retained, which hands back a guard that releases itself
    /// and refuses the shapes it cannot own. This is the raw form and the escape
    /// hatch: taking it means owning the pair by hand, on every path, and
    /// releasing EXACTLY ONCE -- see Object::release, where twice is NOT safe.
    /// Neither this nor release may be called from fk_after_load; that rule is
    /// on Retained, and it is about the retain rather than about the guard.
    #[inline]
    pub fn retain(self) -> Object {
        Object(unsafe { fk_retain(self.0) })
    }

    /// Retains a TRANSIENT handle and wraps the new slot in a guard that
    /// releases on Drop. None when this handle is not one a guard can own.
    ///
    /// The ownership shape, and the reason it exists: retain and release are a
    /// pair a guest has to keep balanced on EVERY path, and the first Rust mod
    /// to hold handles across events leaked on three -- a build that had
    /// retained three handles before a fourth create failed, an early return
    /// past the release, and a helper that returned a null handle its caller
    /// went on holding.
    ///
    /// A GUARD IS BORN AT THE PROMOTION AND NOWHERE ELSE, which is what makes
    /// two guards over one slot unrepresentable rather than merely discouraged.
    /// The host's retain mints a FRESH persistent slot for every TRANSIENT
    /// handle it is handed -- fk_abi.lua's M.retain pops the free list, or takes
    /// the next unused number -- so every guard that exists came from its own
    /// promotion and names a slot nothing else names. None is the answer to
    /// every other case:
    ///
    /// * an ALREADY PERSISTENT handle. The number names a slot in the
    ///   persistent space, and retain is idempotent there: it hands the same
    ///   number back and mints nothing, so a guard over it could not be the
    ///   slot's only owner. Whoever does own that slot, if anyone, owns it by
    ///   hand or through another guard, and both owners would release it -- the
    ///   free list is LIFO, so the second release frees whatever the next
    ///   retain took. The honest verb for that case is adopt rather than
    ///   retain, and this binding does not offer one.
    /// * a GLOBAL. The nine are bound at load and owned by nobody, so a guard
    ///   over one owns nothing and its Drop is a discarded Status::BAD_HANDLE.
    /// * the NULL handle.
    /// * a retain that FAILED and came back 0 -- Status::NO_SPACE, or a
    ///   transient number the host never handed out. A guard over nothing is
    ///   the third leak above rebuilt inside the new API: the caller holds a
    ///   guard and never learns the retain missed.
    ///
    /// WHAT IT CANNOT REFUSE IS A STALE TRANSIENT HANDLE, and no predicate
    /// could. The host restarts the transient counter WHEN THE OUTERMOST
    /// DISPATCH RETURNS, so a number kept from an EARLIER event has been handed
    /// out again and names whatever THIS dispatch put at that index: the retain
    /// SUCCEEDS, and the guard that comes back owns a real slot over somebody
    /// else's object, with no status anywhere. A NESTED dispatch restarts
    /// nothing, so a guard taken before create_entity{raise_built=true} or
    /// entity.die() is unharmed by the event that call raises. This gate is
    /// over the SPACE; the dispatch rule is the caller's, and it is on
    /// is_transient. Retain within the dispatch that produced the handle.
    #[inline]
    pub fn retained(self) -> Option<Retained> {
        if !self.is_transient() {
            return None;
        }
        let slot = self.retain();
        if !slot.is_valid() {
            return None;
        }
        Some(Retained(slot))
    }

    /// Frees a retained handle. Release it EXACTLY ONCE: twice is not safe.
    ///
    /// Releasing a TRANSIENT handle does nothing and is not an error -- the
    /// whole space goes when the outermost dispatch returns. Releasing a
    /// persistent slot a SECOND time is a defect the host cannot catch:
    /// fk_abi.lua checks that the slot is OCCUPIED, not by whom, and the free
    /// list is LIFO, so the very next retain takes that slot back. A stale
    /// release then frees somebody else's object and answers Status::OK,
    /// leaving two live owners naming one slot -- one of them reading an object
    /// it never retained -- with no status anywhere to notice. Hold the handle
    /// in a Retained and this stops being a question the caller has to answer.
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

    /// The number is in the GLOBAL range: one of the nine fixed globals --
    /// game, script, prototypes and the rest.
    ///
    /// A RANGE TEST like the other two, and what it says is about the NUMBER
    /// rather than about the object: 1..9 are never allocated and never freed,
    /// so a global number is never reused for something else -- which is the one
    /// thing the other two ranges do not give you, since a persistent slot is
    /// freed and handed out again and the transient counter restarts when the
    /// outermost dispatch returns.
    ///
    /// IT DOES NOT SAY THE HANDLE IS LIVE. The host resolves a global by NAME
    /// out of the game's own environment on every access, and game does not
    /// exist while control.lua is loading -- which is where a guest's package
    /// initialisers run -- nor inside Factorio's own on_load. A global there
    /// answers Status::BAD_HANDLE and its retain comes back 0. Once the game has
    /// bound them they live for the session: retaining one hands the same number
    /// back, and RELEASING one is Status::BAD_HANDLE, because a guest does not
    /// own them.
    #[inline]
    pub fn is_global(self) -> bool {
        self.0 != 0 && self.0 < Self::FIRST_DYNAMIC
    }

    /// The number NAMES A SLOT IN THE PERSISTENT SPACE: not a global, not
    /// transient.
    ///
    /// A RANGE TEST, NOT AN OWNERSHIP FACT. It is one compare on the number and
    /// no host call, so it cannot say whether this guest retained the slot,
    /// whether the slot is still occupied, or whether the object behind it is
    /// alive. A handle already RELEASED answers true, and so does a number a
    /// guest built with Object(n) and never retained. The only way to learn a
    /// handle is dead is to make a call and read Status::BAD_HANDLE.
    ///
    /// FOR OWNERSHIP, HOLD A Retained. A guard is the only thing that says a
    /// slot is owed a release, because it is the only thing that was there when
    /// the slot was minted. These three predicates answer the SPACE question;
    /// the guard answers the OWNERSHIP one.
    ///
    /// THE NINE GLOBALS ANSWER FALSE HERE, which is a deliberate reading, and
    /// it stands on the range alone: a global is in NEITHER dynamic space. 1..9
    /// sit below FIRST_DYNAMIC and are never allocated or freed, so the three
    /// predicates partition every handle exactly the way fk_abi.lua's own table
    /// does, and the null handle answers false to all three. Folding the globals
    /// in because they also outlive the dispatch would put them in a range they
    /// are not in. Ask is_transient (or its negation on a live handle) for the
    /// outlives-the-dispatch question.
    #[inline]
    pub fn is_persistent(self) -> bool {
        self.0 >= Self::FIRST_DYNAMIC && self.0 < Self::TRANSIENT_BASE
    }

    /// The number is in the TRANSIENT range: the host discards it when the
    /// OUTERMOST dispatch returns. Everything the API hands back starts here.
    ///
    /// A range test like the two above, and it CANNOT TELL A LIVE TRANSIENT
    /// HANDLE FROM A STALE ONE. A number in this range that the host never
    /// handed out names nothing and answers Status::BAD_HANDLE. One LEFT OVER
    /// FROM AN EARLIER DISPATCH is worse than that: the host restarts the
    /// counter WHEN THE OUTERMOST DISPATCH RETURNS (fk_mod.lua's dispatch_done,
    /// which is what reaches fk_abi.lua's M.clear_transient), so that same
    /// number has been handed out AGAIN and names whatever THIS dispatch put at
    /// that index -- a DIFFERENT object, resolving with Status::OK and
    /// retaining successfully into a slot over it. It answers BAD_HANDLE only
    /// when this dispatch has not allocated that far yet. Nothing on this side
    /// can see the difference, because all three predicates are compares on the
    /// number.
    ///
    /// A NESTED DISPATCH RESTARTS NOTHING, and that is deliberate: Factorio
    /// raises some events from inside the API call that caused them --
    /// create_entity{raise_built=true} and entity.die() are the everyday ones --
    /// so fk_mod.lua counts the depth and does the end-of-dispatch work only at
    /// depth 0. A handle an outer handler is holding still names its own object
    /// across such a call.
    ///
    /// So the rule is the caller's: RETAIN WITHIN THE DISPATCH THAT PRODUCED
    /// THE HANDLE -- Object::retained here, Retain in Go -- and keep the guard
    /// rather than the number. A transient number kept across events is
    /// neither a leak nor a dead handle: in a lockstep game it is a desync.
    #[inline]
    pub fn is_transient(self) -> bool {
        self.0 >= Self::TRANSIENT_BASE
    }
}

/// A retained handle that releases itself.
///
/// WHAT IT IS FOR. Every path out of a scope releases, including the ones an
/// author did not think about: an early return, a ? on a failing host call, a
/// loop that breaks. Deref makes it stand in for the Object everywhere, so a
/// generated class type wraps it as LuaEntity(*guard) and every method is
/// reachable through it.
///
/// ONE SLOT, ONE GUARD, BY CONSTRUCTION. The only way to make one is
/// Object::retained -- or Retained::new, the same thing spelled as a
/// constructor -- and it accepts ONLY a transient handle, for which the host
/// mints a fresh persistent slot on every retain. So no two guards can name one
/// slot: the double-owner shape is unrepresentable rather than forbidden by a
/// comment. Not Copy and not Clone for the same reason.
///
/// A COPY TAKEN THROUGH Deref IS A BORROW. *guard is an Object naming the
/// guard's slot, and it is exactly what to pass to a call or wrap in a class
/// type. It must NOT outlive the guard, must NOT be released by hand, and must
/// NOT be stored anywhere the guard's scope does not cover. It cannot be turned
/// into a second guard, because retained() answers None for a handle already in
/// the persistent space.
///
/// ACROSS A SAVE. It lives in the guest's own state, and under --persist=table
/// or packed the guest heap is what Factorio serializes -- so a guard parked in
/// a static map comes back after a load still naming the slot it named, because
/// the host's persistent handle table came back with it. On a REBUILT guest the
/// heap is discarded and the persistent table is discarded under the same gate
/// (agents/abi.md), so the guards die with the numbers they were holding and
/// nothing is left pinned.
///
/// DROP IS DETERMINISTIC because it is guest-driven: it runs where the scope
/// ends, on every peer THAT RUNS THAT SCOPE, in the same order. It is not a
/// finalizer and there is no collector deciding when. Note that panic = "abort"
/// is mandatory for a guest, so NO Drop ever runs during a panic -- a trap takes
/// the mod down without unwinding, and the whole handle table goes with the
/// session.
///
/// NEVER FROM fk_after_load, and that qualifier on the paragraph above is the
/// one rule this type cannot enforce for you. script.on_load runs on the peer
/// that LOADS the state, which on a running server is the joining client and
/// nobody else, while the host's persistent table is aliased into storage and
/// the guard itself lives in storage.fk_mem -- both of them checksummed. So a
/// retain, a release, a Retained constructed or a Retained dropped under
/// fk_after_load (or under anything it computes) is a peer-local write to
/// replicated state, which is a desync rather than a leak. Rebuild caches there
/// and nothing else. See docs/rules.md, "No peer-local signal may change guest
/// state".
#[derive(Debug)]
pub struct Retained(Object);

impl Retained {
    /// Retains a transient handle and takes ownership of the release.
    ///
    /// The same thing as Object::retained, spelled the way a constructor is,
    /// with the same refusals: None for a handle that is already persistent,
    /// for a global, for the null handle, and for a retain that failed.
    #[inline]
    pub fn new(o: Object) -> Option<Retained> {
        o.retained()
    }

    /// Hands the handle back WITHOUT releasing it, for a caller that means to
    /// manage the pair itself. After this the slot is HAND-MANAGED: nothing
    /// will free it but an Object::release, exactly once, and no new guard can
    /// be made over it, because retained() refuses a persistent handle.
    #[inline]
    pub fn into_object(self) -> Object {
        let o = self.0;
        core::mem::forget(self);
        o
    }
}

impl core::ops::Deref for Retained {
    type Target = Object;
    #[inline]
    fn deref(&self) -> &Object {
        &self.0
    }
}

impl Drop for Retained {
    #[inline]
    fn drop(&mut self) {
        self.0.release();
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
    Status(unsafe { fk_subscribe(event, 0, 0, 0, 0) })
}

/// Subscribes to an event addressed by NAME rather than by defines.events,
/// which is how Factorio delivers a CUSTOM INPUT -- the keybind a mod declares
/// with a custom-input prototype at the data stage.
///
///     subscribe_named(EVENT_CUSTOMINPUTEVENT, "my-mod-hotkey");
///
/// THE EVENT ID IS STILL THE PAYLOAD'S and is still a constant at the call site.
/// It says what the handler will be handed -- CustomInputEvent's player_index,
/// input_name, cursor_position and the rest -- and the NAME says what to
/// register under. defines.events.CustomInputEvent does not exist in any
/// Factorio (measured: the table has 233 keys and that is not one of them), so
/// without a name there is nothing to register at all.
///
/// SEVERAL CUSTOM INPUTS SHARE ONE HANDLER, because they all carry the same
/// payload descriptor and therefore the same id. Read input_name out of the
/// payload to tell them apart.
///
/// A name no custom-input prototype in this game has is refused by the ENGINE at
/// subscribe time; it comes back as a status here and as one line in the log
/// carrying the engine's own words, and the mod keeps running.
///
/// #[inline(always)] for the pruning reason its siblings carry -- see
/// subscribe_filtered.
#[inline(always)]
pub fn subscribe_named(event: u32, name: &str) -> Status {
    Status(unsafe { fk_subscribe(event, 0, 0, name.as_ptr() as u32, name.len() as u32) })
}

/// subscribe_named and subscribe_masked at once: registered by name, and the
/// host does not encode the fields this guest will not read. CustomInputEvent
/// has three maskable ones -- SKIP_CUSTOM_INPUT_EVENT_CURSOR_DIRECTION,
/// ..._SELECTED_PROTOTYPE and ..._ELEMENT.
///
/// There is deliberately no named-and-FILTERED form. Factorio's event filters
/// are declared per described event, as the Lua<Event>EventFilter concepts, and
/// a custom input has none -- so the combination would be a binding that exists
/// and always fails.
#[inline(always)]
pub fn subscribe_named_masked(event: u32, skip: u32, name: &str) -> Status {
    Status(unsafe { fk_subscribe(event, 0, skip, name.as_ptr() as u32, name.len() as u32) })
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
    Status(unsafe { fk_subscribe(event, 0, skip, 0, 0) })
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
    Status(unsafe { fk_subscribe(event, p, skip, 0, 0) })
}

/// The descriptor kinds fk.register takes, mirroring fk_mod.lua's REG_COMMAND,
/// REG_INTERFACE and REG_MODEVENT -- and gogen.go's, which is the fourth
/// spelling of the same three numbers and is checked against the other three
/// rather than trusted.
const REG_COMMAND: u32 = 1;
const REG_INTERFACE: u32 = 2;
const REG_MOD_EVENT: u32 = 3;

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

/// Subscribes to an event ANOTHER MOD defined, handled by this guest's
/// fk_on_call export.
///
/// THE SUBSCRIBE HALF OF A PUBLISHED PROTOCOL. A mod publishes an event with
/// script.generate_event_name() and hands the id out through its remote
/// interface; every consumer subscribes to that number. All of the publishing
/// side binds -- generate_event_name, raise_event, get_event_id -- and until
/// this existed the consuming side did not: a runtime-minted id is a NUMBER
/// where subscribe wants a dense index into the generated event table, and a
/// mod-defined event's payload is not in the API description at all, so there
/// is no field descriptor to encode it with.
///
/// const EV_DELIVERY: u32 = 7; // this guest's own dispatch id
///
/// #[no_mangle]
/// pub extern "C" fn _initialize() {
///     if let Ok(v) = fkapi::remote_call("logistic-train-network",
///                                       "on_delivery_completed", &[]) {
///         if let Some(n) = v.as_num() {
///             fkapi::register_mod_event(EV_DELIVERY, n as u32);
///         }
///     }
/// }
///
/// THE PAYLOAD IS ONE TIER-2 VALUE, because there is nothing to type it
/// against: the other end is another mod's table and no description carries its
/// shape. That is why this is fk_on_call rather than fk_on_event -- the id is
/// this guest's own and the argument list is the seam's.
///
/// CALL IT FROM _initialize, exactly as with add_command and for the same
/// reason: a registration is not saved, so it has to be made on every load, and
/// nothing here writes an event id into storage.
pub fn register_mod_event(id: u32, event: u32) -> Status {
    register_mod_event_value(id, Value::Number(event as f64))
}

/// register_mod_event for an event declared as a custom-event PROTOTYPE at the
/// data stage rather than minted at runtime.
///
/// Both are arms of Factorio's own LuaEventType and the host passes either
/// through unchanged. A name no custom-event prototype in this game has is
/// refused by the ENGINE at subscribe time and comes back as StatusNoMember,
/// with the engine's own words in one log line.
pub fn register_mod_event_named(id: u32, name: &str) -> Status {
    register_mod_event_value(id, Value::Str(name.into()))
}

fn register_mod_event_value(id: u32, event: Value) -> Status {
    let _m = AllocMark::new();
    let p = galloc(DYN_W as u32);
    let d = unsafe { core::slice::from_raw_parts_mut(p as *mut u8, DYN_W) };
    write_dyn(
        d,
        &Value::Map(alloc::vec![
            (Value::Str("id".into()), Value::Number(id as f64)),
            (Value::Str("event".into()), event),
        ]),
    );
    Status(unsafe { fk_register(REG_MOD_EVENT, p) })
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

/// The nil a missing lookup answers with. A static rather than a returned
/// Value: get() hands back a reference, so there has to be something for it to
/// point at that outlives the call.
static NIL: Value = Value::Nil;

/// Reading a tier-2 value back.
///
/// There were no accessors in either language, so every read of a tier-2 map
/// was a hand-written scan and a match. THE TWO FAMILIES ARE THE GO BINDING'S,
/// AND THE SPELLING IS RUST'S: a lookup (get, get_key, at) answers with a
/// &Value whose miss is Value::Nil, so it chains; a read (as_bool,
/// as_num, as_str, as_obj) answers with an Option, which is what Go
/// spells as a comma-ok. Forcing one shape onto both languages would have made
/// one of them worse, and the Into-variant precedent already says the
/// rendering may differ where the wire does not.
///
/// NOTHING HERE COERCES. as_num on a string is None rather than a parse.
///
/// A MISS AND A PRESENT NIL ARE DIFFERENT AND get CANNOT TELL YOU WHICH:
/// has is what answers that.
impl Value {
    /// Whether this is the nil value. Value::default() is nil.
    pub fn is_nil(&self) -> bool {
        matches!(self, Value::Nil)
    }

    /// The number of elements in an array or of pairs in a map, and 0 for
    /// everything else.
    pub fn len(&self) -> usize {
        match self {
            Value::Array(a) => a.len(),
            Value::Map(m) => m.len(),
            _ => 0,
        }
    }

    /// Whether [Value::len] is zero. Present because clippy asks for it beside
    /// len, and because "an empty option table" is a real question.
    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    /// Looks a string key up in a map. Not a map, or key absent, is
    /// Value::Nil -- so the result chains.
    ///
    /// A LINEAR SCAN, which is the honest shape for a pair vector: the maps the
    /// API carries are option tables and event payloads with a handful of keys.
    pub fn get(&self, key: &str) -> &Value {
        if let Value::Map(m) = self {
            for (k, v) in m.iter() {
                if let Value::Str(s) = k {
                    if s.as_bytes() == key.as_bytes() {
                        return v;
                    }
                }
            }
        }
        &NIL
    }

    /// get for a key that is not a string -- a number-keyed map. A container
    /// key never matches: no described map is keyed by one.
    pub fn get_key(&self, key: &Value) -> &Value {
        if let Value::Map(m) = self {
            for (k, v) in m.iter() {
                if same_scalar(k, key) {
                    return v;
                }
            }
        }
        &NIL
    }

    /// Whether a map carries this key, which is the one question get cannot
    /// answer: a key present and nil reads exactly like a key that is absent.
    pub fn has(&self, key: &str) -> bool {
        if let Value::Map(m) = self {
            for (k, _) in m.iter() {
                if let Value::Str(s) = k {
                    if s.as_bytes() == key.as_bytes() {
                        return true;
                    }
                }
            }
        }
        false
    }

    /// Indexes an array, zero-based. Out of range, or not an array, is
    /// Value::Nil.
    pub fn at(&self, i: usize) -> &Value {
        if let Value::Array(a) = self {
            if i < a.len() {
                return &a[i];
            }
        }
        &NIL
    }

    /// The payload the variant names, or None for every other variant
    /// INCLUDING nil -- which is what keeps an absent key and a present false
    /// apart.
    pub fn as_bool(&self) -> Option<bool> {
        match self {
            Value::Bool(b) => Some(*b),
            _ => None,
        }
    }

    pub fn as_num(&self) -> Option<f64> {
        match self {
            Value::Number(n) => Some(*n),
            _ => None,
        }
    }

    /// The BYTES, not a str: a Lua string is an arbitrary byte sequence and
    /// [LuaStr] is what this crate carries one in.
    pub fn as_str(&self) -> Option<&LuaStr> {
        match self {
            Value::Str(s) => Some(s),
            _ => None,
        }
    }

    pub fn as_obj(&self) -> Option<Object> {
        match self {
            Value::Obj(o) => Some(*o),
            _ => None,
        }
    }

    /// Those reads with the None spent on a default.
    pub fn bool_or(&self, def: bool) -> bool {
        self.as_bool().unwrap_or(def)
    }

    pub fn num_or(&self, def: f64) -> f64 {
        self.as_num().unwrap_or(def)
    }

    /// Borrows for as long as BOTH the value and the default live, because the
    /// answer is one or the other and a caller cannot say which.
    pub fn str_or<'a>(&'a self, def: &'a [u8]) -> &'a [u8] {
        match self {
            Value::Str(s) => s.as_bytes(),
            _ => def,
        }
    }

    pub fn obj_or(&self, def: Object) -> Object {
        self.as_obj().unwrap_or(def)
    }
}

/// Renders this value into dst and answers how many bytes it wrote.
///
/// A DEBUGGER'S EYES. The accessors above answer a question you already knew to
/// ask; this is for the case where you do not, and the recorded debugging loop
/// for it was recompile, repackage, rerun and diff a transcript.
///
/// INTO A BUFFER THE CALLER OWNS, AND NOT A String. Building one would allocate,
/// and the default bump allocator never gives it back -- which is exactly the
/// cost the fklog crate exists to remove, so a dumper that allocated would undo
/// it at the one call site most likely to be in a loop. fklog lends its own
/// tail for this:
///
/// /// fklog::start("v=");
/// fklog::advance(v.dump(fklog::tail()));
/// fklog::end();
///
/// TRUNCATION OVER GROWTH: a value bigger than dst is cut, and the return is
/// what fits.
///
/// DETERMINISTIC BY CONSTRUCTION. A map's pairs are a Vec and are rendered in
/// the order the host sent them; nothing here iterates a hash map, so two guests
/// on two clients render one value identically.
///
/// The rendering is the Go binding's, byte for byte, and it is Lua-ish rather
/// than JSON: {k=v, ...}, [a, b], quoted strings, #N for a handle. It is for a
/// person reading a log and is not parsed back anywhere.
impl Value {
    pub fn dump(&self, dst: &mut [u8]) -> usize {
        let mut d = Dumper { dst, n: 0 };
        d.value(self);
        d.n
    }
}

struct Dumper<'a> {
    dst: &'a mut [u8],
    n: usize,
}

impl Dumper<'_> {
    fn put(&mut self, x: &[u8]) {
        let room = self.dst.len().saturating_sub(self.n);
        let k = core::cmp::min(room, x.len());
        self.dst[self.n..self.n + k].copy_from_slice(&x[..k]);
        self.n += k;
    }

    fn s(&mut self, x: &str) {
        self.put(x.as_bytes());
    }

    fn value(&mut self, v: &Value) {
        match v {
            Value::Nil => self.s("nil"),
            Value::Bool(b) => self.s(if *b { "true" } else { "false" }),
            Value::Number(f) => self.num(*f),
            Value::Str(x) => {
                self.s("\"");
                let b = x.as_bytes();
                self.put(b);
                self.s("\"");
            }
            Value::Obj(o) => {
                self.s("#");
                self.uint(o.0 as u64);
            }
            Value::Array(a) => {
                self.s("[");
                for (i, e) in a.iter().enumerate() {
                    if i > 0 {
                        self.s(", ");
                    }
                    self.value(e);
                }
                self.s("]");
            }
            Value::Map(m) => {
                self.s("{");
                for (i, (k, val)) in m.iter().enumerate() {
                    if i > 0 {
                        self.s(", ");
                    }
                    // A string key is rendered bare, which is what a Lua table
                    // literal looks like; anything else takes the [k] form.
                    if let Value::Str(ks) = k {
                        let b = ks.as_bytes();
                        self.put(b);
                    } else {
                        self.s("[");
                        self.value(k);
                        self.s("]");
                    }
                    self.s("=");
                    self.value(val);
                }
                self.s("}");
            }
        }
    }

    /// A number the way a diagnostic wants it: integral values with no
    /// fractional part, anything else to three decimal places with trailing
    /// zeroes trimmed. Not a real float formatter, for fklog's reason.
    fn num(&mut self, mut f: f64) {
        if f < 0.0 {
            self.s("-");
            f = -f;
        }
        if !(f < 9.2e18) {
            // NaN reaches here too, and big is a better answer than a number
            // that is not one.
            self.s("big");
            return;
        }
        let mut whole = f as u64;
        let mut frac = ((f - whole as f64) * 1000.0 + 0.5) as u64;
        if frac >= 1000 {
            whole += 1;
            frac -= 1000;
        }
        self.uint(whole);
        if frac == 0 {
            return;
        }
        self.s(".");
        if frac < 100 {
            self.s("0");
        }
        if frac < 10 {
            self.s("0");
        }
        while frac % 10 == 0 {
            frac /= 10;
        }
        self.uint(frac);
    }

    fn uint(&mut self, mut v: u64) {
        let mut b = [0u8; 20];
        let mut i = b.len();
        loop {
            i -= 1;
            b[i] = b'0' + (v % 10) as u8;
            v /= 10;
            if v == 0 {
                break;
            }
        }
        self.put(&b[i..]);
    }
}

/// Equality for a map KEY. A variant mismatch is never equal, and a container
/// is never equal to anything.
fn same_scalar(a: &Value, b: &Value) -> bool {
    match (a, b) {
        (Value::Nil, Value::Nil) => true,
        (Value::Bool(x), Value::Bool(y)) => x == y,
        (Value::Number(x), Value::Number(y)) => x == y,
        (Value::Str(x), Value::Str(y)) => x.as_bytes() == y.as_bytes(),
        (Value::Obj(x), Value::Obj(y)) => x.0 == y.0,
        _ => false,
    }
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
