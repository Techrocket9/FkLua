-- FkLua UDP IPC probe -- what helpers.send_udp, helpers.recv_udp and
-- on_udp_packet_received actually do on the INSTALLED Factorio.
--
-- This is the first thing in this repo that talks to a RUNNING Factorio rather
-- than reading its logs afterwards, and everything it measures gates a protocol
-- constant: the frame-size cap, the pump cadence, whether ordering may be
-- assumed, and whether a payload may contain arbitrary bytes at all.
--
-- THE GATING QUESTION IS WHETHER THIS FILE RUNS AT ALL. A headless
-- `recv_udp(0)` crashed the game at 2.0.x's descendant 2.1.9 -- "Trying to add
-- invalid input action to the closure", TickClosure.cpp:91 -- and whether the
-- 2.0.77 build this repo pins has that defect is unverified either way. It is a
-- C++ crash rather than a Lua error, so the pcall below cannot catch it; what
-- says it happened is the server process dying, which scripts/run-ipcprobe.sh
-- watches for.
--
-- THE MOD IS DRIVEN OVER THE WIRE, not by tick number. Every test is a command
-- sent by scripts/ipc-probe-driver.py, which means the schedule lives on the
-- side that has a clock and the mod never has to guess when the driver is
-- ready. The one exception is `autosend_tick`, which exists for --benchmark:
-- that mode runs the update loop as fast as it can, so a driver cannot react
-- inside it, and an unprompted send is the only way to observe the outbound
-- direction there.
--
-- WIRE FORMAT, both directions: one ASCII header line terminated by "\n",
-- then an arbitrary body.
--
--     <cmd> <reply-port> [args...]\n<body>
--
-- The reply port travels in every command on purpose. A compiled-in port would
-- make the mod and the driver two places to keep one number, which is the
-- shape this repo's own notes call "two commands disagreeing about one
-- manifest key".

local cfg = require("config")

-- ---------------------------------------------------------------------------
-- Logging. Every line is FKIPCPROBE <n> <...>, where n is a per-run monotone
-- counter: the harness greps two files (the server's stdout and
-- factorio-current.log) and has to dedupe without `sort -u`, because ORDER is
-- one of the findings.
-- ---------------------------------------------------------------------------
local logn = 0
local function P(fmt, ...)
  logn = logn + 1
  -- The literal-string form. A bare string in a LocalisedString position is a
  -- LOCALE KEY, which is the same trap this file exists to measure on the
  -- send_udp side.
  log({ "", "FKIPCPROBE " .. logn .. " " .. string.format(fmt, ...) })
end

-- ---------------------------------------------------------------------------
-- Byte helpers. Nothing here may assume a payload is text.
-- ---------------------------------------------------------------------------

-- All 256 byte values, once, so the outbound binary test does not rebuild it.
local BIN256
do
  local t = {}
  for i = 0, 255 do t[#t + 1] = string.char(i) end
  BIN256 = table.concat(t)
end

local function hexs(s)
  return (string.gsub(s, ".", function(c) return string.format("%02x", string.byte(c)) end))
end

-- A cheap order-sensitive checksum that stays exact in a double: the running
-- value is under 2^32 and 2^32 * 31 + 255 is well inside 2^53, so no rounding
-- happens on a doubles-only build. Chunked through string.byte because a
-- per-character gsub over 65,000 bytes is a different kind of measurement.
local function checksum(s)
  local sum, n, i = 2166136261, #s, 1
  while i <= n do
    local j = i + 255
    if j > n then j = n end
    local b = { string.byte(s, i, j) }
    for k = 1, #b do sum = (sum * 31 + b[k]) % 4294967296 end
    i = j + 1
  end
  return sum
end

-- What a payload looks like in one log line without pasting 65 kB into it.
local function digest(s)
  local n = #s
  if n == 0 then return "len=0" end
  return string.format("len=%d first=%02x last=%02x sum=%d", n,
    string.byte(s, 1), string.byte(s, n), checksum(s))
end

-- ---------------------------------------------------------------------------
-- The record that becomes probe.json.
-- ---------------------------------------------------------------------------
local R = {
  meta = {},
  shape = {},     -- the pairs() dump of the first few events
  events = {},    -- one compact row per event received
  errors = {},
  notes = {},
}

local ev_count = 0      -- events seen this run
local recv_err = 0      -- recv_udp calls that raised
local send_err = 0      -- send_udp calls that raised
local ready = false
local mark = 0          -- `reset` sets this to ev_count; `count` reports past it
local first_tick = nil  -- what the scheduled sends are counted from

-- ---------------------------------------------------------------------------
-- Sending. `for_player` and the LocalisedString form are both variables here
-- rather than constants, because both are things this probe is measuring.
--
--   fp = -1     omit the argument entirely (every peer sends its own copy)
--   fp = 0      server only
--   fp = n      that player only
--
--   form "bare"    send_udp(port, s, fp)        s is a LOCALE KEY
--   form "concat"  send_udp(port, {"", s}, fp)  s is a literal
-- ---------------------------------------------------------------------------
local function send(port, s, form, fp)
  form = form or cfg.send_form
  if fp == nil then fp = cfg.for_player end
  local data = s
  if form == "concat" then data = { "", s } end
  local ok, err
  if fp == -1 then
    ok, err = pcall(helpers.send_udp, port, data)
  else
    ok, err = pcall(helpers.send_udp, port, data, fp)
  end
  if not ok then
    send_err = send_err + 1
    R.errors["send:" .. form .. ":" .. tostring(fp) .. ":" .. #s] = tostring(err)
    P("SENDERR port=%d form=%s fp=%d len=%d %s", port, form, fp, #s, tostring(err))
  end
  return ok
end

-- ---------------------------------------------------------------------------
-- The complete event shape, which is answer 2 and cannot be taken from the
-- docs: what is asserted here is what pairs() yields, with the type of every
-- value, plus the metatable, plus the documented names read directly so that
-- "absent" and "present but nil" are distinguishable.
-- ---------------------------------------------------------------------------
local function dump_shape(e)
  local rows = {}
  for k, v in pairs(e) do
    rows[#rows + 1] = { k = tostring(k), kt = type(k), v = v }
  end
  table.sort(rows, function(a, b) return a.k < b.k end)
  for _, r in ipairs(rows) do
    local vt = type(r.v)
    local sv
    if vt == "string" then sv = digest(r.v) else sv = tostring(r.v) end
    P("SHAPE ev=%d key=%s keytype=%s valtype=%s value=%s", ev_count, r.k, r.kt, vt, sv)
    R.shape[#R.shape + 1] = { ev = ev_count, key = r.k, keytype = r.kt, valtype = vt, value = sv }
  end
  P("SHAPEMETA ev=%d meta=%s nkeys=%d", ev_count, tostring(getmetatable(e)), #rows)
  -- The documented names, read by index rather than found by iteration. A field
  -- the docs name and pairs() does not yield would show up as a difference
  -- between these two lines.
  P("SHAPEDOC ev=%d name=%s tick=%s player_index=%s source_port=%s payload=%s",
    ev_count, tostring(e.name), tostring(e.tick), tostring(e.player_index),
    tostring(e.source_port), type(e.payload) == "string" and digest(e.payload) or tostring(e.payload))
end

-- ---------------------------------------------------------------------------
-- The commands. Each takes the already-split header words and the body.
-- ---------------------------------------------------------------------------
local cmds = {}

-- ping <port> <token>  -> pong <tick> <token>
-- The latency leg. The token is opaque to the mod; the driver puts its own
-- clock reading in it and closes the loop on the way back.
cmds.ping = function(port, a, _, e)
  send(port, "pong " .. e.tick .. " " .. (a[3] or "-"))
end

-- echo <port> [form] [fp] -> the body, verbatim
-- The outbound binary-safety leg, and the one that answers what for_player and
-- the LocalisedString form do, because both are arguments here.
cmds.echo = function(port, a, body)
  local form = a[3]
  local fp = tonumber(a[4] or "")
  send(port, body, form, fp)
end

-- hex <port> <tag> -> hex <tag> <len> <hexstring>
-- The INBOUND binary-safety leg. It never sends the received bytes back as
-- bytes, so a mangled reply cannot be blamed on the outbound path.
cmds.hex = function(port, a, body)
  local tag = a[3] or "-"
  if #body > 1024 then
    send(port, "hex " .. tag .. " " .. #body .. " TOOBIG")
  else
    send(port, "hex " .. tag .. " " .. #body .. " " .. hexs(body))
  end
end

-- len <port> <tag> -> len <tag> <n> <first> <last> <sum>
-- The inbound SIZE leg: a datagram that arrives is described rather than
-- returned, so the answer is not capped by whatever the outbound size limit
-- turns out to be.
cmds.len = function(port, a, body)
  local tag = a[3] or "-"
  local first, last = 0, 0
  if #body > 0 then first, last = string.byte(body, 1), string.byte(body, #body) end
  send(port, string.format("len %s %d %d %d %d", tag, #body, first, last, checksum(body)))
end

-- big <port> <n> <kind> <tag> -> n bytes
-- The outbound SIZE leg. kind `a` is printable, kind `b` is 0..255 repeating.
cmds.big = function(port, a)
  local n = tonumber(a[3] or "") or 0
  local kind = a[4] or "a"
  local tag = a[5] or "-"
  local head = "big " .. tag .. " " .. n .. "\n"
  local body
  if kind == "b" then
    body = string.sub(string.rep(BIN256, math.ceil(n / 256) + 1), 1, n)
  else
    body = string.sub(string.rep("ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", math.ceil(n / 36) + 1), 1, n)
  end
  P("BIGSEND tag=%s n=%d kind=%s %s", tag, n, kind, digest(body))
  send(port, head .. body)
end

-- forms <port> <tag> -> four sends of the same payload, one per shape
-- This is the one that decides how the guest library must call send_udp. The
-- data parameter is a LocalisedString, so a bare string is a LOCALE KEY and a
-- payload that is not a known key may come out as "Unknown key:" text -- or
-- not, which is exactly what has never been measured.
cmds.forms = function(port, a, body)
  local tag = a[3] or "-"
  local payload = (#body > 0) and body or "the quick brown fox"
  send(port, "F1 " .. tag .. "\n" .. payload, "bare", cfg.for_player)
  send(port, "F2 " .. tag .. "\n" .. payload, "concat", cfg.for_player)
  -- A three-element concat, because the wire shape of a multi-part literal is
  -- what a guest that builds a frame from a header and a body would produce.
  -- THE fp SENTINEL IS HONOURED HERE TOO. This leg used to pass cfg.for_player
  -- verbatim, so in any arm configured fp = -1 it called send_udp(port, data, -1)
  -- and errored on the sentinel itself -- and the resulting SENDERR was briefly
  -- misread as an engine finding about the three-part form (2026-08-07). It was
  -- this probe's own artifact; the form had never been measured in those arms.
  local data3 = { "", "F3 " .. tag .. "\n", payload }
  local ok
  if cfg.for_player == -1 then
    ok = pcall(helpers.send_udp, port, data3)
  else
    ok = pcall(helpers.send_udp, port, data3, cfg.for_player)
  end
  if not ok then send_err = send_err + 1; P("SENDERR form=concat3 tag=%s", tag) end
  -- for_player OMITTED. On a server with no players this is the "every peer
  -- sends its own copy" form, and whether that reaches the socket at all when
  -- the only peer is the server is a real question.
  send(port, "F4 " .. tag .. "\n" .. payload, "concat", -1)
end

-- fp <port> <tag> -> one send per for_player value
-- for_player = 1 with nobody connected is the interesting one: silently
-- skipped, an error, or delivered anyway.
cmds.fp = function(port, a)
  local tag = a[3] or "-"
  send(port, "FP0 " .. tag, "concat", 0)
  send(port, "FPOMIT " .. tag, "concat", -1)
  send(port, "FP1 " .. tag, "concat", 1)
end

-- seq <port> <i> -> nothing
-- The burst leg. Recorded like any other packet; `count` reads the record back.
cmds.seq = function() end

-- reset <port> -> reset ok
cmds.reset = function(port)
  mark = ev_count
  send(port, "reset ok " .. mark)
end

-- count <port> -> count <total> <since-mark> ;i:tick:sport:len:note,...
-- What arrived since the last `reset`, in the order the mod saw it. This is
-- the ordering and batching answer; the ticks in it are the batching one.
cmds.count = function(port)
  local out = {}
  for i = mark + 1, ev_count do
    local r = R.events[i]
    if r then
      out[#out + 1] = string.format("%d:%d:%d:%d:%s", i, r.tick, r.source_port, r.len, r.note or "-")
    end
  end
  send(port, string.format("count %d %d ", ev_count, ev_count - mark) .. table.concat(out, ","))
end

-- stat <port> -> one line of everything the mod knows about itself
cmds.stat = function(port, _, _, e)
  send(port, string.format(
    "stat tick=%d events=%d recverr=%d senderr=%d evid=%s pump=%s form=%s fp=%d players=%d",
    e.tick, ev_count, recv_err, send_err, tostring(defines.events.on_udp_packet_received),
    cfg.pump, cfg.send_form, cfg.for_player, #game.players))
end

-- dump <port> -> dump ok
-- BOTH write_file forms, because for_player=0 on a run with no server is the
-- documented silent skip and the only way to see it is a file that is not there.
local function write_dump(tick)
  R.meta.tick = tick
  R.meta.events = ev_count
  R.meta.recv_err = recv_err
  R.meta.send_err = send_err
  R.meta.event_id = defines.events.on_udp_packet_received
  R.meta.pump = cfg.pump
  R.meta.send_form = cfg.send_form
  R.meta.for_player = cfg.for_player
  local json = helpers.table_to_json(R)
  local ok0 = pcall(helpers.write_file, "fkipc/probe.json", json, false, 0)
  local okA = pcall(helpers.write_file, "fkipc/probe-all.json", json, false)
  P("DUMPED tick=%d bytes=%d for_player0=%s omitted=%s", tick, #json, tostring(ok0), tostring(okA))
  return ok0, okA
end

cmds.dump = function(port, _, _, e)
  local ok0, okA = write_dump(e.tick)
  send(port, string.format("dump ok fp0=%s omit=%s events=%d", tostring(ok0), tostring(okA), ev_count))
end

-- ---------------------------------------------------------------------------
-- THE UNPROMPTED OUTBOUND SUITE, on the mod's own tick clock.
--
-- On 2.0.77 a headless server dies the moment a packet is RECEIVED, and the
-- send half underneath that is healthy -- so a request/response probe measures
-- nothing about sending on the one environment the server profile runs in.
-- These are the same command bodies the driver would have asked for, invoked
-- from on_tick instead, with the driver listening in silence.
--
-- Ages are ticks after the mod's first tick, spaced far enough apart that a
-- 65,000-byte send and a four-way form test never share one.
local PLAN = {
  { 30,  function() cmds.forms(cfg.reply_port, { "forms", "-", "ascii" },
                               "the quick brown fox jumps over 0123456789") end },
  -- The same four shapes carrying all 256 byte values, which is the question
  -- that decides whether a frame may be binary at all: send_udp takes a
  -- LocalisedString, and a locale key is not an obvious place to put a NUL.
  { 60,  function() cmds.forms(cfg.reply_port, { "forms", "-", "bin" }, BIN256) end },
  { 90,  function() cmds.fp(cfg.reply_port, { "fp", "-", "auto" }) end },
  { 120, function() cmds.big(cfg.reply_port, { "big", "-", "256", "b", "bin256" }) end },
}

-- THE SIZE LADDER, appended to the plan rather than written out, because
-- finding the ceiling took two passes and the second one only changed this
-- list. The first pass measured 8,192 arriving and 16,384 not, with NO error
-- from send_udp either time -- so the rungs between them are where the cap is,
-- and 9,188 is there because a datagram's payload plus its 28 bytes of IP and
-- UDP header is exactly macOS's default 9,216-byte UDP send buffer.
local SIZES = { 1400, 4000, 8000, 8192, 8500, 9000, 9160, 9188, 9216,
                9500, 10000, 12000, 16384, 65000, 65507 }
do
  local age = 150
  for _, n in ipairs(SIZES) do
    PLAN[#PLAN + 1] = { age, function()
      cmds.big(cfg.reply_port, { "big", "-", tostring(n), "a", "o" .. n })
    end }
    age = age + 30
  end

  -- Ten sends in ONE tick: whether the game will emit more than one datagram
  -- per tick at all, and whether they stay in order.
  PLAN[#PLAN + 1] = { age, function()
    for i = 1, 10 do send(cfg.reply_port, string.format("burstout %d", i)) end
  end }
  age = age + 30

  -- IS A BARE STRING LOCALISED? send_udp's data parameter is a
  -- LocalisedString, so a plain string is documented as a locale KEY, and the
  -- form test above came back byte-exact -- which says a key that resolves to
  -- nothing is passed through, not that nothing is looked up. These two send a
  -- key that REALLY EXISTS, in both forms and nothing else in the datagram, so
  -- a translated arrival is unmistakable: "Continue" rather than
  -- "gui-menu.continue".
  PLAN[#PLAN + 1] = { age, function()
    P("KEYTEST bare gui-menu.continue")
    send(cfg.reply_port, "gui-menu.continue", "bare")
  end }
  age = age + 30
  PLAN[#PLAN + 1] = { age, function()
    P("KEYTEST concat gui-menu.continue")
    send(cfg.reply_port, "gui-menu.continue", "concat")
  end }
  age = age + 30

  PLAN[#PLAN + 1] = { age, function() send(cfg.reply_port, "planend") end }
end

-- ---------------------------------------------------------------------------
-- The event handler.
-- ---------------------------------------------------------------------------
local function on_udp(e)
  ev_count = ev_count + 1
  local payload = e.payload
  local plen = (type(payload) == "string") and #payload or -1

  -- The complete shape, for the first three only: it is four log lines per
  -- event and the burst leg sends twenty.
  if ev_count <= 3 then dump_shape(e) end

  local head, body = "", ""
  if type(payload) == "string" then
    local nl = string.find(payload, "\n", 1, true)
    if nl then head, body = string.sub(payload, 1, nl - 1), string.sub(payload, nl + 1)
    else head, body = payload, "" end
  end

  local a = {}
  for w in string.gmatch(head, "%S+") do a[#a + 1] = w end
  local cmd = a[1] or "-"
  local port = tonumber(a[2] or "") or cfg.reply_port

  local note = cmd
  if cmd == "seq" then note = "seq" .. (a[3] or "?") end

  R.events[ev_count] = {
    i = ev_count, tick = e.tick, source_port = e.source_port,
    player_index = e.player_index, len = plen, note = note,
    sum = (type(payload) == "string") and checksum(payload) or -1,
  }

  P("EV %d tick=%d source_port=%s player_index=%s cmd=%s %s",
    ev_count, e.tick, tostring(e.source_port), tostring(e.player_index), cmd,
    (type(payload) == "string") and digest(payload) or ("payloadtype=" .. type(payload)))

  local fn = cmds[cmd]
  if fn then
    local ok, err = pcall(fn, port, a, body, e)
    if not ok then
      R.errors["cmd:" .. cmd] = tostring(err)
      P("CMDERR cmd=%s %s", cmd, tostring(err))
    end
  end
end

script.on_event(defines.events.on_udp_packet_received, on_udp)

-- ---------------------------------------------------------------------------
-- The pump. Nothing arrives without it, and the 256 KB OS buffer is dropped
-- silently, so the cadence is a correctness parameter rather than a tuning one.
-- Every tick here because this is a probe: what a real guest should do is one
-- of the things the numbers this produces are for.
-- ---------------------------------------------------------------------------
script.on_event(defines.events.on_tick, function(e)
  if not ready then
    ready = true
    first_tick = e.tick
    P("READY tick=%d pump=%s form=%s fp=%d reply_port=%d",
      e.tick, cfg.pump, cfg.send_form, cfg.for_player, cfg.reply_port)
    P("SURFACE send_udp=%s recv_udp=%s write_file=%s table_to_json=%s",
      type(helpers.send_udp), type(helpers.recv_udp),
      type(helpers.write_file), type(helpers.table_to_json))
    -- Answer 9's half: the runtime id of the event, against the 207 the
    -- generated bindings carry. Never hardcode it -- ids are dense per-version
    -- indices -- but a probe may certainly print it.
    P("EVENTID on_udp_packet_received=%s", tostring(defines.events.on_udp_packet_received))
    -- WHO IS IN THIS GAME, which is the leading explanation for everything the
    -- receive side does here. `for_player = 0` means "the server if present"
    -- and an absent player index selects nobody, so an environment with
    -- neither a player nor a server has nothing for recv_udp to read FOR.
    local ok, n = pcall(function()
      return string.format("players=%d connected=%d multiplayer=%s",
        #game.players, #game.connected_players, tostring(game.is_multiplayer()))
    end)
    P("WHOSHERE %s", ok and n or ("unavailable: " .. tostring(n)))
  end

  if cfg.pump ~= "none" then
    local ok, err
    if cfg.pump == "bare" then ok, err = pcall(helpers.recv_udp)
    elseif cfg.pump == "one" then ok, err = pcall(helpers.recv_udp, 1)
    else ok, err = pcall(helpers.recv_udp, 0) end
    if not ok then
      recv_err = recv_err + 1
      if recv_err <= 5 then
        R.errors["recv:" .. recv_err] = tostring(err)
        P("RECVERR tick=%d n=%d %s", e.tick, recv_err, tostring(err))
      end
    end
  end

  -- The unprompted send, for an arm nothing can handshake with. Counted from
  -- the first tick THIS RUN saw rather than from zero, because a map that has
  -- been through a headless server already starts partway in.
  local age = e.tick - first_tick
  if cfg.autosend_after > 0 and age == cfg.autosend_after then
    P("AUTOSEND tick=%d age=%d port=%d", e.tick, age, cfg.reply_port)
    send(cfg.reply_port, string.format("auto %d %d %d", e.tick, ev_count, recv_err))
  end

  if cfg.outbound_suite then
    for _, p in ipairs(PLAN) do
      if age == p[1] then
        P("PLAN age=%d", age)
        local ok, err = pcall(p[2])
        if not ok then P("PLANERR age=%d %s", age, tostring(err)) end
      end
    end
  end

  if cfg.dump_after > 0 and age == cfg.dump_after then write_dump(e.tick) end
end)
