// Command api is the M7 end-to-end guest: a Go program that calls the Factorio
// API through the generated bindings.
//
// It is the fixture internal/guest builds, and it is deliberately small: what
// it proves is that the whole chain connects -- runtime-api.json, the member
// table, the handle table, the marshalling, and TinyGo's dead-code elimination
// leaving only the members actually called.
//
//	tinygo build -target=wasm-unknown -scheduler=none -gc=leaking -o api.wasm ./examples/api
//	fklua mod api.wasm --name fk-api --version 0.1.0 --author you
package main

import (
	"strconv"

	"github.com/Techrocket9/fklua/guest/go/fk"
	"github.com/Techrocket9/fklua/guest/go/fkapi"
)

// Event ids come from the generated package. They are per-build -- adding an
// event renumbers the list -- so writing one by hand is a bug waiting for the
// next regeneration. This example did exactly that once, and started
// subscribing to whatever had moved into the old number.
const (
	evOnPlayerCreated     = fkapi.EventOnPlayerCreated
	evOnTick              = fkapi.EventOnTick
	evOnBuiltEntity       = fkapi.EventOnBuiltEntity
	evOnRobotBuiltEntity  = fkapi.EventOnRobotBuiltEntity
	evOnPlayerMinedEntity = fkapi.EventOnPlayerMinedEntity
	evOnRobotMinedEntity  = fkapi.EventOnRobotMinedEntity
)

var ticksSeen uint32

// profSink is what the profiler leg below times. A package-level sink because
// -opt=2 would otherwise delete a loop whose result nothing reads, and a
// profiler around no work reports a duration that says nothing.
var profSink int

//go:wasmexport fk_on_event
func onEvent(id, ptr uint32) {
	switch id {
	case evOnPlayerCreated:
		// A GENERATED STRUCT, not a cast at a hand-derived offset. This example
		// used to read `*(*uint32)(unsafe.Pointer(uintptr(ptr)))` with the
		// layout in a comment -- and fields are placed by the API's `order`,
		// so one new optional field upstream shifts everything after it and the
		// guest quietly reads a neighbouring value instead.
		e := fkapi.ReadOnPlayerCreated(ptr)
		fk.Log("event: player created, index " +
			strconv.FormatUint(uint64(e.PlayerIndex), 10))

	case evOnTick:
		// The tick arrives as an encoded field rather than a call argument,
		// which is the difference between this path and the legacy fk_on_tick
		// hook.
		ticksSeen++
		if ticksSeen == 20 {
			e := fkapi.ReadOnTick(ptr)
			fk.Log("event: on_tick #" + strconv.FormatUint(uint64(ticksSeen), 10) +
				" carries tick " + strconv.FormatUint(e.Tick, 10))
		}
	}
}

//go:wasmexport fk_on_init
func onInit() {
	fk.Log("api example: reaching Factorio from Go")

	// A DEFINE, ASKED FOR RATHER THAN WRITTEN DOWN. defines.direction.east is
	// 4 in this Factorio and that number is not in the API description at all
	// -- runtime-api.json carries the name and the order, never the value --
	// so a hand-written 4 is a guess that happens to be right today. The
	// accessor resolves it by name at load and caches it; the compiler proves
	// the id constant and ships one path out of the whole set (`define_values`
	// in census.json).
	fk.Log("defines.direction.east = " +
		strconv.FormatUint(uint64(fkapi.DefinesDirectionEast()), 10))
}

func init() {
	// Subscribing from an initialiser is what a real guest does: it runs during
	// _initialize, before any event can fire.
	fkapi.Subscribe(evOnPlayerCreated)
	fkapi.Subscribe(evOnTick)

	// ...AND FOUR WITH FACTORIO'S OWN FILTERS, which the engine applies in C++
	// before the guest is entered.
	//
	// FOUR RATHER THAN ONE, AND THE COUNT IS THE TEST. These are here for the
	// PRUNING as much as for the filtering: fklua mod ships only the event
	// descriptors it can prove a guest subscribes to, by scanning the wasm for a
	// constant reaching the import, and it is all-or-nothing -- one id it cannot
	// prove and the whole table ships. SubscribeFiltered is several times the
	// size of Subscribe, and on the Rust arm that difference was enough for the
	// id to arrive as a runtime parameter once there were four call sites --
	// 85 KB of extra Lua per load, reported from the field by a downstream
	// Rust port. TinyGo inlines both wrappers here; this is the
	// guest that keeps saying so, gated by
	// TestTheEventIdSurvivesTheGeneratedSubscribeWrapper.
	onlyChests := fkapi.NameFilter("iron-chest")
	fkapi.SubscribeFiltered(evOnBuiltEntity, onlyChests...)
	fkapi.SubscribeFiltered(evOnRobotBuiltEntity, onlyChests...)
	fkapi.SubscribeFiltered(evOnPlayerMinedEntity, onlyChests...)

	// ...AND THE FOURTH BY prototype TYPE RATHER THAN BY NAME, which is the
	// other filter helper and the one nothing exercised until now.
	//
	// It is the same event count and the same wire shape -- one map term, two
	// keys -- so nothing calibrated moves; what it demonstrates is the choice.
	// `iron-chest` is one prototype and `container` is every chest there is,
	// including ones a mod added, which is why a guest that means "any chest"
	// should not be writing names. Terms OR together within a call, so the
	// mixed form is append(NameFilter(...), TypeFilter(...)...).
	fkapi.SubscribeFiltered(evOnRobotMinedEntity, fkapi.TypeFilter("container")...)
}

//go:wasmexport fk_on_tick
func onTick(tick uint32) {
	// Once, rather than sixty times a second: this is a demonstration, and a
	// mod that logs every tick is a mod nobody keeps installed.
	if tick != 30 {
		return
	}

	// `game` is handle 2, fixed by the ABI. Every other handle is reached by
	// calling something.
	speed, err := fkapi.Game.Speed()
	if err != nil {
		// A host call never raises into wasm -- there are no coroutines, so an
		// error crossing that boundary could not unwind the frame it came from.
		// It arrives as a Go error instead.
		fk.Log("reading game.speed failed: " + err.Error())
		return
	}
	fk.Log("game.speed = " + strconv.FormatFloat(float64(speed), 'f', 2, 32))

	tick64, err := fkapi.Game.Tick()
	if err == nil {
		fk.Log("game.tick = " + strconv.FormatUint(uint64(tick64), 10))
	}

	// Writing works the same way, and the round trip proves the value really
	// crossed rather than being read back out of the guest's own buffer.
	if err := fkapi.Game.SetSpeed(speed * 2); err != nil {
		fk.Log("writing game.speed failed: " + err.Error())
		return
	}
	doubled, _ := fkapi.Game.Speed()
	fk.Log("game.speed doubled = " + strconv.FormatFloat(float64(doubled), 'f', 2, 32))

	// A HOST-SIDE STRING PREDICATE. `surface.name` is a string, and asking
	// whether it EQUALS one never brings the string across: the comparison
	// happens in Lua and a bool comes back, so the guest keeps nothing. Under
	// -gc=leaking that is the difference between 0 and 48 bytes per call,
	// forever -- and this is the path a category-filtered event handler is on
	// for every entity anyone builds.
	if s, err := fkapi.Game.GetSurface(fkapi.OfNumber(1)); err == nil && s != nil {
		surface := fkapi.LuaSurface{Object: *s}
		isNauvis, err := surface.NameIs("nauvis")
		if err != nil {
			fk.Log("surface.NameIs failed: " + err.Error())
		} else {
			fk.Log("surface.name == \"nauvis\" is " + strconv.FormatBool(isNauvis) +
				", and no string crossed")
		}

		// ...and a CONTAINER RETURN into a destination the caller keeps. Same
		// member id and same host call as FindEntitiesFiltered; the slice is
		// the only difference, and it is the whole ~1.3 KB a downstream
		// network compile was leaving behind.
		ents, err := surface.FindEntitiesFilteredInto(entBuf, fkapi.EntitySearchFilters{})
		if err != nil {
			fk.Log("FindEntitiesFilteredInto failed: " + err.Error())
		} else {
			entBuf = ents
			fk.Log("find_entities_filtered into a reused buffer: " +
				strconv.Itoa(len(ents)) + " entities, cap " + strconv.Itoa(cap(ents)))
		}

		// A CLASS OPERATOR: the Lua `it()`, which is LuaChunkIterator's whole
		// useful surface and was unreachable until the generators learned to
		// read Class.Operators. Bound as Call() -- an operator has no name to
		// resolve, so the ABI dispatches on the kind alone.
		if it, err := surface.GetChunks(); err == nil {
			ci := fkapi.LuaChunkIterator{Object: it}
			c, err := ci.Call()
			switch {
			case err != nil:
				fk.Log("chunk iterator failed: " + err.Error())
			case c == nil:
				fk.Log("chunk operator: iterator was already done")
			default:
				fk.Log("chunk operator: first chunk " +
					strconv.FormatInt(int64(c.X), 10) + "," +
					strconv.FormatInt(int64(c.Y), 10))
			}
		}

		// ...and the INDEX and LENGTH operators, `#inv` and `inv[1]`, on a
		// chest this makes for the purpose. A headless run has no player, so
		// there is no inventory lying about to read.
		chest, err := surface.CreateEntity(fkapi.OfMap(
			fkapi.KeyValue{Key: fkapi.OfString("name"), Val: fkapi.OfString("iron-chest")},
			fkapi.KeyValue{Key: fkapi.OfString("position"), Val: fkapi.OfArray(
				fkapi.OfNumber(8), fkapi.OfNumber(8))},
			fkapi.KeyValue{Key: fkapi.OfString("force"), Val: fkapi.OfString("player")},
		))
		if err != nil || chest == nil {
			fk.Log("create_entity(iron-chest) did not produce one")
		} else if inv, err := (fkapi.LuaEntity{Object: *chest}).
			GetInventory(fkapi.DefinesInventoryChest()); err == nil && inv != nil {
			box := fkapi.LuaInventory{Object: *inv}
			n, lerr := box.Length()
			slot, ierr := box.Get(1)
			if lerr != nil || ierr != nil {
				fk.Log("inventory operators failed")
			} else {
				fk.Log("inventory operators: #inv = " +
					strconv.FormatUint(uint64(n), 10) + ", inv[1] valid " +
					strconv.FormatBool(slot.Valid()))
			}
		}

		// THE INDEX OPERATOR'S WRITE HALF, `t[k] = v`, which is the only way a
		// mod changes its own runtime-global setting:
		//
		//	settings.global["my-setting"] = {value = true}
		//
		// Two calls, and the first is the whole point of the handle route:
		// GlobalRaw hands back the LuaCustomTable itself rather than
		// materialising every setting in the game in order to write one.
		//
		// THIS MOD DECLARES NO SETTINGS, so what the engine answers is its
		// refusal -- "LuaCustomTable doesn't contain key" -- and that is the leg
		// worth having in a real game: a Factorio metamethod raising has to come
		// back as a STATUS, never as an unwind through the wasm frame the call
		// came from. A mod with a setting of its own gets OK here and the
		// setting changes, per save.
		if raw, err := fkapi.Settings.GlobalRaw(); err != nil {
			fk.Log("settings.global as a handle failed: " + err.Error())
		} else {
			err = fkapi.LuaCustomTable{Object: raw}.Set(
				fkapi.OfString("fklua-no-such-setting"),
				fkapi.OfMap(fkapi.KeyValue{
					Key: fkapi.OfString("value"), Val: fkapi.OfBool(true)}),
			)
			fk.Log("index-assign: settings.global[undefined] refused " +
				strconv.FormatBool(err != nil))
		}

		// A GLOBAL FUNCTION, and the one that made the kind worth building:
		// `log()` is the ONLY way to read a LuaProfiler's duration. The class
		// has add, divide, reset, restart, stop, object_name, object_name_is and
		// valid -- not one of them returns the number -- and the engine renders
		// it only when the profiler is an ELEMENT of a LocalisedString.
		//
		//	local p = helpers.create_profiler()
		//	...work...
		//	p.stop()
		//	log{"", "[marker] ", p}
		//
		// What lands in factorio-current.log is `... Duration: 12.368959ms`, and
		// a downstream harness regexes exactly that. There is no other shape:
		// fk.Log takes a plain string and a plain string cannot carry an object.
		if p, err := fkapi.Helpers.CreateProfiler(nil); err != nil {
			fk.Log("create_profiler failed: " + err.Error())
		} else {
			// Something to time. The work is beside the point; that the ENGINE
			// renders the elapsed figure is the whole leg.
			for i := 0; i < 2000; i++ {
				profSink += i
			}
			if err := (fkapi.LuaProfiler{Object: p}).Stop(); err != nil {
				fk.Log("profiler stop failed: " + err.Error())
			}
			if err := fkapi.Log(fkapi.OfArray(
				fkapi.OfString(""),
				fkapi.OfString("global-fn: profiler "),
				fkapi.OfObject(p),
			)); err != nil {
				fk.Log("global-fn: log() failed: " + err.Error())
			}
			// ...and table_size, which is the global function with a RETURN. A
			// three-key table the guest built itself, so the answer is known.
			n, err := fkapi.TableSize(fkapi.OfMap(
				fkapi.KeyValue{Key: fkapi.OfString("a"), Val: fkapi.OfNumber(1)},
				fkapi.KeyValue{Key: fkapi.OfString("b"), Val: fkapi.OfNumber(2)},
				fkapi.KeyValue{Key: fkapi.OfString("c"), Val: fkapi.OfNumber(3)},
			))
			if err != nil {
				fk.Log("global-fn: table_size failed: " + err.Error())
			} else {
				fk.Log("global-fn: table_size = " + strconv.FormatUint(uint64(n), 10))
			}
		}

		// A MEMBER RETURNING SEVERAL VALUES, which the generators deferred for
		// four milestones on naming rules alone -- and this one is the ONLY way
		// to arm on_object_destroyed, so no Go or Rust guest could subscribe to
		// that event in any useful sense.
		reg, useful, kind, err := fkapi.Script.RegisterOnObjectDestroyed(
			fkapi.OfObject(*s))
		if err != nil {
			fk.Log("register_on_object_destroyed failed: " + err.Error())
		} else {
			fk.Log("multi-return: registration " +
				strconv.FormatUint(reg, 10) + " useful " +
				strconv.FormatUint(useful, 10) + " kind " +
				strconv.FormatUint(uint64(kind), 10))
		}
	}
}

// The destination the Into call reuses. A package-level buffer is the shape a
// real mod wants: allocated once, refilled every call, never handed to anything
// that outlives the next one.
var entBuf []fkapi.Object

func main() {}
