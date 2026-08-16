package fkipc

import "github.com/Techrocket9/fklua/guest/go/fkipc/wire"

// channel is one channel's state: its two seq counters, its handlers, and the
// at-most-one reassembly it may have open.
type channel struct {
	id  uint16
	pri Priority

	txSeq  uint32
	rxLast uint32

	// resyncSent, and no separate "gap" flag beside it: a flag nothing reads is
	// bytes in the save. What a gap MEANS to this channel is entirely "a RESYNC
	// is outstanding", which is what gates sending another one.
	resyncSent bool

	onMessage func(Message)
	onRequest func(Request) []byte
	onResync  func()
	onGap     func(uint32)

	rasmActive   bool
	rasmCorr     uint32
	rasmDeadline uint32
	rasmType     wire.Type
	rasmFlags    wire.Flags
	rasmNFrag    uint8
	rasmGot      uint8
	rasmSeen     [wire.MaxFragments]bool
	rasmPart     [wire.MaxFragments][]byte
}

// nextSeq is per CHANNEL and per DIRECTION, and it counts FRAMES rather than
// messages -- so a lost fragment is a detectable gap instead of a silently
// short message.
//
// Channel 0 is the protocol's own and never carries one: a lost heartbeat is
// normal and must not read as a gap in application state.
func (c *channel) nextSeq() uint32 {
	if c.id == 0 {
		return 0
	}
	c.txSeq++
	return c.txSeq
}

// abandon drops whatever reassembly is open. The part buffers keep their
// capacity, because the next message on this channel is the same size as the
// last one far more often than not.
func (c *channel) abandon() {
	c.rasmActive = false
	c.rasmGot = 0
	c.rasmCorr = 0
	c.rasmNFrag = 0
	for i := range c.rasmSeen {
		c.rasmSeen[i] = false
		c.rasmPart[i] = c.rasmPart[i][:0]
	}
}

// Channel names one channel of one link.
//
// It is a value, and the link it belongs to is inside it, so a Channel obtained
// before a Reload still names the same channel afterwards -- Reload resets a
// channel's counters, it does not forget the channel.
type Channel struct {
	l  *Link
	id uint16
}

// Chan names a channel, creating it the first time.
//
// 0 is the protocol's own; 1-65535 are the application's. Naming one twice
// returns the same channel and updates its priority, so registration order --
// which is deterministic, being the same code on every peer -- decides nothing
// that reaches the wire.
func Chan(id uint16, pri Priority) Channel { return pkg.Chan(id, pri) }

func (l *Link) Chan(id uint16, pri Priority) Channel {
	if l == nil {
		return Channel{}
	}
	if c := l.findChan(id); c != nil {
		c.pri = pri
		return Channel{l: l, id: id}
	}
	c := &channel{id: id, pri: pri}
	// Sorted insert. The order is deterministic either way -- a guest's
	// registrations are the same on every peer -- but sorted means a lookup can
	// stop early and a dump of the table reads the same as the wire.
	at := len(l.chans)
	for i, e := range l.chans {
		if e.id > id {
			at = i
			break
		}
	}
	l.chans = append(l.chans, nil)
	copy(l.chans[at+1:], l.chans[at:])
	l.chans[at] = c
	return Channel{l: l, id: id}
}

func (l *Link) findChan(id uint16) *channel {
	for _, c := range l.chans {
		if c.id == id {
			return c
		}
		if c.id > id {
			return nil
		}
	}
	return nil
}

// ID is the channel's wire number.
func (c Channel) ID() uint16 { return c.id }

// Send queues one MSG.
//
// The payload is COPIED into the library's frame buffer before returning, so
// the caller may reuse its slice -- which is the point, because the shape a
// guest wants is one scratch buffer refilled every tick rather than an
// allocation per message in a heap that is in the save.
//
// With no peer this is a COUNTED NO-OP rather than an error to handle at every
// call site. The mod's own behaviour must be defined with no peer, and this is
// the library making that the easy path.
func (c Channel) Send(payload []byte) Status { return c.send(payload, 0) }

// Snapshot is Send with the SNAPSHOT flag: a complete state rather than a
// delta, which is the ONLY answer to a RESYNC.
//
// A gap is never answered with a replay, and the reason is not economy: the
// producer usually CANNOT replay, because the state it described no longer
// exists. A resend of "entity 4471 at 30% health" three seconds later is a lie,
// and a lie that arrives is worse than a gap that is noticed. There is no
// retransmit queue anywhere in this design.
func (c Channel) Snapshot(payload []byte) Status {
	return c.send(payload, wire.FlagSnapshot)
}

func (c Channel) send(payload []byte, flags wire.Flags) Status {
	l := c.l
	if l == nil {
		return StatusNotOpen
	}
	if !l.enabled {
		return l.refused()
	}
	if !l.up {
		l.stats.QueueDrops++
		return StatusNoSession
	}
	ch := l.findChan(c.id)
	if ch == nil {
		return StatusNotOpen
	}
	return l.sendMessage(ch, wire.TypeMsg, flags, 0, payload)
}

// Request queues a REQ and registers the completion.
//
// THERE ARE NO GOROUTINES AND NO CHANNELS ON THIS TARGET, so every asynchronous
// result in FkLua arrives as a callback from a dispatch and this is no
// different. onReply is called from inside a later Pump -- with the answer,
// with ErrTimeout when the retry budget runs out, or with ErrSessionLost if the
// session ends first, which means THE OUTCOME IS UNKNOWN rather than "it
// failed".
//
// The same corr is retried on the schedule; the responder keys its dedup table
// on (epoch, channel, corr) and replays rather than re-invoking its handler. So
// a request must be IDEMPOTENT in the sense that asking twice is safe, which is
// the whole bargain this protocol asks for.
func (c Channel) Request(payload []byte, onReply func(Reply)) (Corr, Status) {
	l := c.l
	if l == nil {
		return 0, StatusNotOpen
	}
	if !l.enabled {
		return 0, l.refused()
	}
	if !l.up {
		l.stats.QueueDrops++
		return 0, StatusNoSession
	}
	ch := l.findChan(c.id)
	if ch == nil {
		return 0, StatusNotOpen
	}
	p := l.allocPending()
	if p == nil {
		return 0, StatusTooManyPending
	}
	corr := l.nextCorr()
	p.ch, p.corr, p.onReply = c.id, corr, onReply
	p.msg = append(p.msg[:0], payload...)
	p.tries = 0
	if st := l.sendMessage(ch, wire.TypeReq, 0, corr, payload); st != StatusOK {
		l.freePending(p)
		return 0, st
	}
	p.interval = l.retryTicks()
	p.due = l.tick + p.interval
	return Corr(corr), StatusOK
}

// The inbound handlers.
//
// THE PAYLOAD HANDED TO ANY OF THESE IS A VIEW into the library's own buffer
// and is invalid the moment the handler returns. Copy what you keep. Same rule
// as a transient handle and the host's string scratch region, for the same
// reason.

func (c Channel) OnMessage(h func(m Message)) {
	if ch := c.state(); ch != nil {
		ch.onMessage = h
	}
}

// OnRequest's return value is the RESP payload, and it is copied before it goes
// on the wire. A nil return is an empty response, not an error: an error is
// something the handler cannot express here on purpose -- an application error
// belongs inside the payload, where the application already has an encoding for
// it, and the protocol's own error codes are about the PROTOCOL.
func (c Channel) OnRequest(h func(r Request) []byte) {
	if ch := c.state(); ch != nil {
		ch.onRequest = h
	}
}

// OnResync is "send me a snapshot". A channel with no handler simply does not
// answer, which is right for a channel that carries no replayable state.
func (c Channel) OnResync(h func()) {
	if ch := c.state(); ch != nil {
		ch.onResync = h
	}
}

// OnGap reports how many frames were missed. The library has already sent the
// RESYNC by the time this runs -- the handler is for the application's own
// accounting, not for it to decide.
func (c Channel) OnGap(h func(missed uint32)) {
	if ch := c.state(); ch != nil {
		ch.onGap = h
	}
}

func (c Channel) state() *channel {
	if c.l == nil {
		return nil
	}
	return c.l.findChan(c.id)
}

// WriteBulk writes data to script-output/<name> and sends a FILE_NOTIFY on c,
// with a length and an FNV-1a-32 the peer can verify exactly.
//
// PREFER IT TO A FRAGMENTED MESSAGE FOR ANYTHING ABOVE ONE FRAME. It is one
// datagram instead of sixteen, and the transport is localhost-only, so the peer
// is always on this filesystem -- the file path is ALWAYS available outbound.
// It is also the only path for a screenshot, which the engine writes to
// script-output and raises no completion event for.
//
// The notify is a MSG-class frame: seq'd, gap-detectable, and NOT retried. The
// file is durable, so a lost notify is recoverable by a RESYNC or by the peer
// scanning the directory; retrying it would be retrying a claim about a file
// that may since have been overwritten.
func WriteBulk(c Channel, name string, data []byte) Status {
	l := c.l
	if l == nil {
		return StatusNotOpen
	}
	// BEFORE THE WRITE, WHICH IS THE ONE PLACE THIS ORDER IS INTERESTING. The
	// file write is a per-instance side effect a guest may make anywhere and the
	// notify is replicated bookkeeping, so the rule is normally "do both, never
	// branch between them". A disabled link does NEITHER: the notify can never
	// be sent, and a write whose announcement is impossible is a file the peer
	// -- which is not running, on an engine with no working IPC -- will never
	// hear about. Refusing both keeps the pair together, which is the invariant
	// the rule is really about.
	if !l.enabled {
		return l.refused()
	}
	if !l.up {
		l.stats.QueueDrops++
		return StatusNoSession
	}
	// ATTEMPT, THEN NOTIFY, UNCONDITIONALLY -- and the seam makes that the only
	// thing this can do, because [Transport.WriteFile] returns nothing.
	//
	// WRITEBULK IS THE PATTERN FOR A PER-INSTANCE SIDE EFFECT, and it is worth
	// copying rather than only reading. Whether write_file works is a fact about
	// this peer: 2.1 documents a non-zero for_player as silently skipped from
	// some stages, and a client is not the server. Branching on it would not
	// merely miscount -- returning early here would SKIP l.notify, which
	// consumes this channel's seq, so one peer would advance the counter and the
	// other would not. That is guest state diverging AND a permanent gap at the
	// far end, from one `if err != nil { return }`.
	//
	// So the shape is: do the local thing, then do the replicated bookkeeping
	// with no edge between them. A guest that wants to know whether its own
	// write landed asks fk.Log, which writes to the game log -- which is not
	// CRC'd, is per-peer by nature, and is where a per-peer fact belongs.
	l.tr.WriteFile(name, data)
	return l.notify(c, wire.FileNotify{
		Bytes: uint32(len(data)), FNV1a32: wire.FNV1a32(data), Name: name,
	}, wire.FlagHasDigest)
}

// NotifyFile announces a file this guest did NOT write -- a screenshot.
//
// No digest, because the guest has never held the bytes and cannot describe
// them, so the peer must stabilize-poll: size unchanged across two polls.
// Nothing documents a flush guarantee for the engine's own writes either, which
// is why the peer's test is a test rather than a promise.
//
// IT IS THE SAME PATTERN AS [WriteBulk] AND IT IS WORTH SEEING WHY. The thing
// that produced the file -- take_screenshot, say -- is a per-instance side
// effect whose success is a fact about this peer, and it is made from wherever
// the guest wants; the notify is replicated bookkeeping and consumes this
// channel's seq. Nothing may sit between them, so the guest calls the one and
// then unconditionally calls the other. Whether the screenshot happened is a
// question for fk.Log and the game's own log, which is not CRC'd.
func NotifyFile(c Channel, name string) Status {
	l := c.l
	if l == nil {
		return StatusNotOpen
	}
	if !l.enabled {
		return l.refused()
	}
	if !l.up {
		l.stats.QueueDrops++
		return StatusNoSession
	}
	return l.notify(c, wire.FileNotify{Name: name}, 0)
}

func (l *Link) notify(c Channel, fn wire.FileNotify, flags wire.Flags) Status {
	ch := l.findChan(c.id)
	if ch == nil {
		return StatusNotOpen
	}
	var err error
	l.ctl, err = wire.AppendFileNotify(l.ctl[:0], fn)
	if err != nil {
		return StatusTooLarge
	}
	return l.sendMessage(ch, wire.TypeFileNotify, flags, 0, l.ctl)
}
