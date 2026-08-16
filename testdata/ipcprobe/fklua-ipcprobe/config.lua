-- The default arm. scripts/run-ipcprobe.sh OVERWRITES this file per arm, so the
-- values here are only what the mod does if somebody runs it by hand.
--
-- pump          how the tick handler calls the receive pump:
--                 "zero" -> helpers.recv_udp(0)   the server socket
--                 "bare" -> helpers.recv_udp()    everything
--                 "one"  -> helpers.recv_udp(1)   player 1, who may not exist
--                 "none" -> no pump at all        the control: nothing arrives
-- for_player    what every reply passes as send_udp's third argument;
--               -1 means OMIT the argument entirely, which is a different call.
-- send_form     how a reply's bytes are handed to send_udp, which takes a
--               LocalisedString and NOT a string:
--                 "bare"   -> send_udp(port, s, fp)          s IS the locale key
--                 "concat" -> send_udp(port, {"", s}, fp)    s is a literal
-- autosend_after one unprompted send this many ticks after the mod's FIRST
--               tick, so an arm that cannot be handshaken (--benchmark runs
--               faster than a driver can react) still proves the outbound
--               direction. 0 disables.
-- dump_after    write the JSON record this many ticks in, unasked. 0 disables;
--               the `dump` command does it on demand.
--
-- BOTH ARE OFFSETS AND NOT ABSOLUTE TICKS, which cost a whole arm before it was
-- fixed: a headless server SAVES THE MAP ON EXIT, so a map reused across arms
-- starts each one later than the last (0, then 1556, then 3116...). An absolute
-- `autosend_tick = 30` fired in the first arm and in no other, and the arm that
-- was supposed to prove send_udp safe proved only that it was never called.
--
-- THE HAND-RUN DEFAULT IS THE SAFE ARM ON 2.0.77, deliberately. `pump = "zero"`
-- against a headless server on this build KILLS THE GAME the moment a packet
-- arrives (TickClosure.cpp:91, see the script's header), so the configuration
-- somebody gets by copying this mod somewhere and running it is the outbound
-- suite with no pump at all. The harness sets `pump` explicitly for the arms
-- whose whole purpose is to provoke that.
return {
  reply_port = 28614,
  pump = "none",
  for_player = 0,
  send_form = "concat",
  autosend_after = 30,
  dump_after = 0,
  -- The UNPROMPTED outbound suite, on a tick schedule. It exists because a
  -- headless server on 2.0.77 dies the moment it RECEIVES anything, and the
  -- send half is perfectly healthy underneath that: driving the sends from the
  -- mod's own clock is the only way to measure the outbound direction in the
  -- environment the server profile actually uses.
  outbound_suite = true,
}
