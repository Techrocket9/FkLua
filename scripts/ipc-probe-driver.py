#!/usr/bin/env python3
"""The other end of testdata/ipcprobe -- the process Factorio talks to.

scripts/run-ipcprobe.sh starts a Factorio with `--enable-lua-udp <game-port>`
and runs this against it. Everything here is a datagram on 127.0.0.1: this
script owns the clock, so the mod never has to guess when the driver is ready
and the whole schedule lives on the side that can measure it.

`--enable-lua-udp <port>` binds ONE socket. That port is the game's receive
socket AND the source port of everything it sends, so this script must listen
on a DIFFERENT one.

Modes:

  server      the full suite, against a headless server. Bidirectional, so it
              handshakes first and gives up loudly if it cannot.
  benchmark   `factorio --benchmark` runs the update loop as fast as it can, so
              nothing here can react inside a tick. It listens for the mod's
              unprompted send and blasts inbound traffic the whole time; the
              verdict is read out of the log afterwards, plus whatever replies
              happen to land.
  silent      LISTENS AND SENDS NOTHING. Half of the crash disambiguation: with
              no inbound traffic at all, anything that goes wrong in the game is
              the game's own send or its own pump, not a packet we delivered.
  blast       sends `seq` packets, which the mod records and does NOT answer.
              The other half: inbound traffic with no outbound reply, so a
              crash under it belongs to the receive path alone.
  outbound    listens while the MOD runs its own send schedule off on_tick.
              This is how the send half gets measured on a headless server,
              which on 2.0.77 cannot survive receiving anything.

Exit status is 0 whenever the probe RAN. A finding is not a failure -- "headless
recv_udp does not work on this build" is the most valuable thing this can
report, and it must not look like a broken harness.
"""

import argparse
import json
import socket
import sys
import time

HOST = "127.0.0.1"


class Driver:
    def __init__(self, game_port, listen_port, verbose=True):
        self.game = (HOST, game_port)
        self.listen_port = listen_port
        self.verbose = verbose
        self.sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        # A 65,000-byte reply needs room, and so does a 20-packet burst that
        # nothing has read yet. The game asks the OS for 256 KB on its side.
        try:
            self.sock.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 1 << 21)
            self.sock.setsockopt(socket.SOL_SOCKET, socket.SO_SNDBUF, 1 << 21)
        except OSError:
            pass
        self.sock.bind((HOST, listen_port))
        self.results = {}

    def say(self, *a):
        if self.verbose:
            print(*a, flush=True)

    # -- the wire ----------------------------------------------------------
    def send(self, header, body=b""):
        """`<cmd> <reply-port> [args]\\n<body>`, which is the whole protocol."""
        pkt = header.encode("ascii") + b"\n" + body
        self.sock.sendto(pkt, self.game)
        return pkt

    def drain(self, seconds=0.3):
        """Everything already queued, so a phase never reads the last one's mail."""
        out = []
        end = time.perf_counter() + seconds
        self.sock.settimeout(0.05)
        while time.perf_counter() < end:
            try:
                out.append(self.sock.recvfrom(70000))
            except socket.timeout:
                pass
        return out

    def collect(self, seconds, want=None):
        """Datagrams for up to `seconds`, stopping early once `want` have landed."""
        out = []
        end = time.perf_counter() + seconds
        while True:
            left = end - time.perf_counter()
            if left <= 0:
                break
            self.sock.settimeout(min(left, 0.25))
            try:
                data, addr = self.sock.recvfrom(70000)
            except socket.timeout:
                continue
            out.append((data, addr))
            if want is not None and len(out) >= want:
                break
        return out

    def ask(self, header, body=b"", timeout=3.0, want=1):
        """One command, and whatever comes back inside the timeout."""
        self.send(header, body)
        return self.collect(timeout, want=want)

    # -- phases ------------------------------------------------------------
    def handshake(self, timeout=90.0):
        """Repeat a ping until a pong lands. Also the first observation of the
        game's SOURCE PORT, which is what answers whether it really is the
        --enable-lua-udp port."""
        self.say(f"==> handshake: pinging {self.game[1]} from {self.listen_port}")
        end = time.perf_counter() + timeout
        n = 0
        while time.perf_counter() < end:
            n += 1
            got = self.ask(f"ping {self.listen_port} hs{n}", timeout=1.0)
            for data, addr in got:
                if data.startswith(b"pong"):
                    self.results["handshake"] = {
                        "ok": True,
                        "attempts": n,
                        "reply": data.decode("ascii", "replace")[:120],
                        "source_addr": addr[0],
                        "source_port": addr[1],
                        "source_port_is_game_port": addr[1] == self.game[1],
                    }
                    self.say(f"    pong after {n} attempt(s): {data[:80]!r} from {addr}")
                    return True
        self.results["handshake"] = {"ok": False, "attempts": n}
        self.say(f"    NO REPLY after {n} attempts -- the game never sent anything back")
        return False

    def phase_shape(self):
        """The event shape is dumped into the LOG by the mod; what this adds is
        a stat line, which is the mod telling us what it thinks it is."""
        got = self.ask(f"stat {self.listen_port}", timeout=3.0)
        line = got[0][0].decode("ascii", "replace") if got else ""
        self.results["stat"] = line
        self.say(f"==> stat: {line}")

    def phase_binary_in(self):
        """Bytes 0..255 INBOUND, described back as hex rather than echoed, so a
        mangled answer cannot be blamed on the outbound path."""
        body = bytes(range(256))
        got = self.ask(f"hex {self.listen_port} b256", body, timeout=4.0)
        rec = {"sent_len": len(body), "arrived": False}
        for data, _ in got:
            if data.startswith(b"hex "):
                parts = data.decode("ascii", "replace").split()
                rec["arrived"] = True
                rec["reported_len"] = int(parts[2]) if len(parts) > 2 else None
                hx = parts[3] if len(parts) > 3 else ""
                rec["hex_matches"] = (hx == body.hex())
                rec["hex_head"] = hx[:64]
                if not rec["hex_matches"] and hx != "TOOBIG":
                    try:
                        back = bytes.fromhex(hx)
                        rec["first_diff"] = next(
                            (i for i in range(min(len(back), len(body))) if back[i] != body[i]),
                            len(back),
                        )
                        rec["returned_len"] = len(back)
                    except ValueError:
                        rec["parse_error"] = True
        self.results["binary_inbound"] = rec
        self.say(f"==> binary inbound 0..255: {rec}")

    def phase_binary_out(self):
        """The same 256 bytes back OUT, twice: echoed from what we sent, and
        generated inside the mod. If only one survives, the direction that broke
        it is named."""
        body = bytes(range(256))
        got = self.ask(f"echo {self.listen_port}", body, timeout=4.0)
        echoed = got[0][0] if got else b""
        rec = {
            "echo_arrived": bool(got),
            "echo_len": len(echoed),
            "echo_exact": echoed == body,
            "echo_hex_head": echoed[:32].hex(),
        }
        if echoed and echoed != body:
            rec["echo_first_diff"] = next(
                (i for i in range(min(len(echoed), len(body))) if echoed[i] != body[i]),
                min(len(echoed), len(body)),
            )
        got = self.ask(f"big {self.listen_port} 256 b gen", timeout=4.0)
        gen = got[0][0] if got else b""
        payload = gen.split(b"\n", 1)[1] if b"\n" in gen else b""
        rec["gen_arrived"] = bool(got)
        rec["gen_len"] = len(payload)
        rec["gen_exact"] = payload == body
        rec["gen_hex_head"] = payload[:32].hex()
        self.results["binary_outbound"] = rec
        self.say(f"==> binary outbound 0..255: {rec}")

    def phase_forms(self):
        """send_udp's data parameter is a LocalisedString, so a bare string is a
        LOCALE KEY. Four shapes of the same payload; whichever come back intact
        are the ones a guest library may use."""
        body = b"the quick brown fox jumps over 0123456789"
        got = self.ask(f"forms {self.listen_port} f1", body, timeout=4.0, want=4)
        rec = {"received": len(got), "forms": {}}
        for data, _ in got:
            head, _, rest = data.partition(b"\n")
            tag = head.split()[0].decode("ascii", "replace") if head.split() else "?"
            rec["forms"][tag] = {
                "len": len(rest),
                "exact": rest == body,
                "text": rest[:80].decode("ascii", "replace"),
            }
        self.results["localised_string_forms"] = rec
        self.say(f"==> send_udp LocalisedString forms: {json.dumps(rec)}")

    def phase_for_player(self):
        """for_player = 0 / omitted / 1, on a run with nobody connected."""
        got = self.ask(f"fp {self.listen_port} p1", timeout=4.0, want=3)
        seen = [d.decode("ascii", "replace").split()[0] for d, _ in got]
        rec = {"received": seen,
               "fp0": "FP0" in seen, "omitted": "FPOMIT" in seen, "fp1": "FP1" in seen}
        self.results["for_player"] = rec
        self.say(f"==> for_player: {rec}")

    def phase_sizes_in(self, sizes):
        """Inbound datagram sizes. The mod DESCRIBES what arrived instead of
        returning it, so this is not capped by the outbound limit."""
        rec = {}
        for n in sizes:
            body = bytes((i * 7 + 13) & 0xFF for i in range(n))
            expect_sum = _checksum(body)
            try:
                got = self.ask(f"len {self.listen_port} s{n}", body, timeout=4.0)
            except OSError as e:
                rec[n] = {"send_failed": str(e)}
                self.say(f"    inbound {n:6d}: send failed: {e}")
                continue
            row = {"arrived": False}
            for data, _ in got:
                p = data.decode("ascii", "replace").split()
                if len(p) >= 6 and p[0] == "len" and p[1] == f"s{n}":
                    row = {"arrived": True, "reported_len": int(p[2]),
                           "len_exact": int(p[2]) == n,
                           "sum_exact": int(p[5]) == expect_sum}
            rec[n] = row
            self.say(f"    inbound {n:6d}: {row}")
        self.results["sizes_inbound"] = rec

    def phase_sizes_out(self, sizes):
        """Outbound datagram sizes: whether the game will emit one at all, and
        whether it arrives whole."""
        rec = {}
        for n in sizes:
            got = self.ask(f"big {self.listen_port} {n} a o{n}", timeout=4.0)
            row = {"arrived": False}
            for data, _ in got:
                head, _, rest = data.partition(b"\n")
                p = head.decode("ascii", "replace").split()
                if len(p) >= 3 and p[0] == "big" and p[1] == f"o{n}":
                    row = {"arrived": True, "requested": n, "received_len": len(rest),
                           "whole": len(rest) == n}
            rec[n] = row
            self.say(f"    outbound {n:6d}: {row}")
        self.results["sizes_outbound"] = rec

    def phase_burst(self, count=20):
        """Many packets between two polls. What comes back says how many the
        pump dispatched, whether they landed on ONE tick, and whether the order
        the mod saw is the order they were sent."""
        self.ask(f"reset {self.listen_port}", timeout=3.0)
        self.drain(0.2)
        t0 = time.perf_counter()
        for i in range(count):
            self.send(f"seq {self.listen_port} {i}")
        blast_ms = (time.perf_counter() - t0) * 1000.0
        time.sleep(1.0)
        got = self.ask(f"count {self.listen_port}", timeout=4.0)
        rec = {"sent": count, "blast_ms": round(blast_ms, 3), "reply": None}
        for data, _ in got:
            if data.startswith(b"count "):
                rec["reply"] = data.decode("ascii", "replace")
        if rec["reply"]:
            # count <total> <since-mark> i:tick:sport:len:note,...
            parts = rec["reply"].split(" ", 3)
            rows = []
            if len(parts) > 3 and parts[3].strip():
                for r in parts[3].strip().split(","):
                    f = r.split(":")
                    if len(f) >= 5:
                        rows.append({"i": int(f[0]), "tick": int(f[1]),
                                     "sport": int(f[2]), "len": int(f[3]), "note": f[4]})
            seqs = [int(r["note"][3:]) for r in rows if r["note"].startswith("seq")
                    and r["note"][3:].isdigit()]
            ticks = sorted({r["tick"] for r in rows if r["note"].startswith("seq")})
            rec["arrived"] = len(seqs)
            rec["in_order"] = seqs == sorted(seqs)
            rec["is_identity"] = seqs == list(range(count))
            rec["distinct_ticks"] = ticks
            rec["one_tick"] = len(ticks) <= 1
            rec["seqs"] = seqs
        self.results["burst"] = rec
        self.say(f"==> burst of {count} in {blast_ms:.2f} ms: "
                 f"arrived={rec.get('arrived')} one_tick={rec.get('one_tick')} "
                 f"in_order={rec.get('in_order')} ticks={rec.get('distinct_ticks')}")

    def phase_latency(self, n=20, gap=0.12):
        """Wall-clock round trip. Rough on purpose: the quantity that matters is
        how many ticks of floor there is, and a tick is 16.67 ms."""
        rtts, ticks = [], []
        for i in range(n):
            t0 = time.perf_counter()
            self.send(f"ping {self.listen_port} L{i}")
            got = self.collect(2.0, want=1)
            t1 = time.perf_counter()
            for data, _ in got:
                if data.startswith(b"pong"):
                    p = data.decode("ascii", "replace").split()
                    rtts.append((t1 - t0) * 1000.0)
                    if len(p) > 1:
                        ticks.append(int(p[1]))
            time.sleep(gap)
        rec = {"samples": len(rtts)}
        if rtts:
            s = sorted(rtts)
            rec.update({
                "min_ms": round(s[0], 3), "median_ms": round(s[len(s) // 2], 3),
                "p90_ms": round(s[int(len(s) * 0.9)], 3), "max_ms": round(s[-1], 3),
                "mean_ms": round(sum(s) / len(s), 3),
                "median_ticks": round(s[len(s) // 2] / 16.667, 2),
                "tick_span": [ticks[0], ticks[-1]] if ticks else None,
            })
        self.results["latency"] = rec
        self.say(f"==> latency: {rec}")

    def phase_source_port(self):
        """A packet from a DIFFERENT socket, so source_port is observed against
        a port we chose rather than against the one we always use."""
        s2 = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        s2.bind((HOST, 0))
        other = s2.getsockname()[1]
        self.ask(f"reset {self.listen_port}", timeout=3.0)
        self.drain(0.2)
        s2.sendto(f"seq {self.listen_port} 999\n".encode(), self.game)
        time.sleep(0.7)
        got = self.ask(f"count {self.listen_port}", timeout=4.0)
        rec = {"other_port": other, "reported": None}
        for data, _ in got:
            if data.startswith(b"count "):
                txt = data.decode("ascii", "replace")
                rec["reply"] = txt
                for r in txt.split(" ", 3)[3].strip().split(","):
                    f = r.split(":")
                    if len(f) >= 5 and f[4] == "seq999":
                        rec["reported"] = int(f[2])
        rec["matches"] = rec["reported"] == other
        s2.close()
        self.results["source_port"] = rec
        self.say(f"==> source_port: sender used {other}, mod saw {rec['reported']} "
                 f"(match={rec['matches']})")

    def phase_dump(self):
        got = self.ask(f"dump {self.listen_port}", timeout=6.0)
        line = got[0][0].decode("ascii", "replace") if got else ""
        self.results["dump"] = line
        self.say(f"==> dump: {line}")


def _checksum(b):
    """The mod's checksum, reproduced exactly so the two can be compared."""
    s = 2166136261
    for x in b:
        s = (s * 31 + x) % 4294967296
    return s


def run_server_suite(d, args):
    if not d.handshake(timeout=args.handshake_timeout):
        d.results["verdict"] = "NO REPLY -- the game never answered a ping"
        return False
    d.drain(0.3)
    d.phase_shape()
    d.phase_binary_in()
    d.phase_binary_out()
    d.phase_forms()
    d.phase_for_player()
    d.phase_sizes_in([1400, 4000, 8192, 16384, 65000])
    d.phase_sizes_out([1400, 4000, 8000, 8192, 16384, 65000])
    d.phase_burst(args.burst)
    d.phase_latency(args.latency_samples)
    d.phase_source_port()
    d.phase_dump()
    d.results["verdict"] = "the game answered"
    return True


def run_benchmark_probe(d, args):
    """--benchmark runs as fast as it can, so this cannot converse. It listens
    for the mod's unprompted send and keeps inbound traffic flowing; the log is
    the durable half of the answer."""
    d.say(f"==> benchmark mode: listening on {d.listen_port}, blasting for {args.seconds}s")
    got_any, first = [], None
    end = time.perf_counter() + args.seconds
    i = 0
    while time.perf_counter() < end:
        i += 1
        d.send(f"len {d.listen_port} bm{i}", b"x" * 64)
        d.send(f"ping {d.listen_port} bm{i}")
        for data, addr in d.collect(0.05):
            if first is None:
                first = (data[:60].decode("ascii", "replace"), addr[1])
            got_any.append(data[:60].decode("ascii", "replace"))
    d.results["benchmark"] = {
        "sent_pairs": i,
        "replies": len(got_any),
        "first_reply": first,
        "sample": got_any[:8],
    }
    d.say(f"==> benchmark: sent {i} pairs, {len(got_any)} datagram(s) came back; first={first}")
    return True


def run_outbound(d, args):
    """The mod runs its own send schedule and this only LISTENS.

    A headless 2.0.77 server dies the instant it receives a packet, so a
    request/response probe cannot measure the send half in the one environment
    the server profile runs in. Nothing is transmitted from here: everything
    below is classified out of what the game emitted unprompted."""
    d.say(f"==> outbound: listening on {d.listen_port} for {args.seconds}s, sending NOTHING")
    got = d.collect(args.seconds)
    ascii_body = b"the quick brown fox jumps over 0123456789"
    b256 = bytes(range(256))

    forms, sizes, fps, bursts, other = {}, {}, {}, [], []
    order = []
    for data, addr in got:
        head, _, body = data.partition(b"\n")
        w = head.split()
        tag = w[0].decode("ascii", "replace") if w else "?"
        order.append(tag)
        if tag in ("F1", "F2", "F3", "F4"):
            which = w[1].decode("ascii", "replace") if len(w) > 1 else "?"
            want = b256 if which == "bin" else ascii_body
            forms[f"{tag}/{which}"] = {
                "len": len(body), "expected_len": len(want), "exact": body == want,
                "head": body[:24].hex() if which == "bin" else
                        body[:40].decode("ascii", "replace"),
                "source_port": addr[1],
            }
        elif tag.startswith("FP"):
            fps[tag] = True
        elif tag == "big":
            which = w[1].decode("ascii", "replace") if len(w) > 1 else "?"
            n = int(w[2]) if len(w) > 2 and w[2].isdigit() else -1
            row = {"requested": n, "received": len(body), "whole": len(body) == n,
                   "source_port": addr[1]}
            if which == "bin256":
                row["exact_bytes"] = body == b256
            sizes[which] = row
        elif tag == "burstout":
            bursts.append(int(w[1]) if len(w) > 1 else -1)
        else:
            other.append(head[:60].decode("ascii", "replace"))

    d.results["outbound"] = {
        "datagrams": len(got),
        "source_ports": sorted({a[1] for _, a in got}),
        "forms": forms,
        "for_player": {"fp0": "FP0" in fps, "omitted": "FPOMIT" in fps, "fp1": "FP1" in fps},
        "sizes": sizes,
        "burst": {"received": bursts, "count": len(bursts),
                  "in_order": bursts == sorted(bursts),
                  "complete": sorted(bursts) == list(range(1, 11))},
        "other": other,
        "arrival_order": order,
    }
    d.say(f"==> outbound: {len(got)} datagram(s) from ports "
          f"{sorted({a[1] for _, a in got})}")
    for k, v in sorted(forms.items()):
        d.say(f"    form {k:12s} len={v['len']:5d} want={v['expected_len']:5d} exact={v['exact']}")
    for k, v in sorted(sizes.items(), key=lambda kv: kv[1]['requested']):
        d.say(f"    size {k:8s} requested={v['requested']:6d} received={v['received']:6d} "
              f"whole={v['whole']}" + (f" bytes_exact={v['exact_bytes']}" if 'exact_bytes' in v else ""))
    d.say(f"    for_player: {d.results['outbound']['for_player']}")
    d.say(f"    burst of 10 in one tick: {d.results['outbound']['burst']}")
    return True


def run_silent(d, args):
    """Nothing outbound from here. Whatever lands is the game talking first."""
    d.say(f"==> silent: listening on {d.listen_port} for {args.seconds}s, sending nothing")
    got = d.collect(args.seconds)
    d.results["silent"] = {
        "received": len(got),
        "sample": [x[0][:60].decode("ascii", "replace") for x in got[:8]],
        "source_ports": sorted({x[1][1] for x in got}),
    }
    d.say(f"==> silent: {len(got)} datagram(s) arrived unprompted")
    return True


def run_blast(d, args):
    """Inbound only. `seq` is the one command the mod records without answering,
    so a game that dies under this died on the RECEIVE path."""
    d.say(f"==> blast: {args.seconds}s of inbound `seq` packets, no reply expected")
    end = time.perf_counter() + args.seconds
    i = 0
    while time.perf_counter() < end:
        i += 1
        d.send(f"seq {d.listen_port} {i}")
        time.sleep(0.05)
    back = d.collect(1.0)
    d.results["blast"] = {
        "sent": i,
        "unexpected_replies": len(back),
        "sample": [x[0][:60].decode("ascii", "replace") for x in back[:8]],
    }
    d.say(f"==> blast: sent {i}, {len(back)} unexpected datagram(s) came back")
    return True


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--game-port", type=int, required=True)
    ap.add_argument("--listen-port", type=int, required=True)
    ap.add_argument("--mode",
                    choices=["server", "benchmark", "silent", "blast", "outbound"],
                    default="server")
    ap.add_argument("--out", default="")
    ap.add_argument("--seconds", type=float, default=20.0)
    ap.add_argument("--burst", type=int, default=20)
    ap.add_argument("--latency-samples", type=int, default=20)
    ap.add_argument("--handshake-timeout", type=float, default=90.0)
    args = ap.parse_args()

    d = Driver(args.game_port, args.listen_port)
    d.results["mode"] = args.mode
    d.results["game_port"] = args.game_port
    d.results["listen_port"] = args.listen_port
    try:
        if args.mode == "server":
            run_server_suite(d, args)
        elif args.mode == "benchmark":
            run_benchmark_probe(d, args)
        elif args.mode == "silent":
            run_silent(d, args)
        elif args.mode == "outbound":
            run_outbound(d, args)
        else:
            run_blast(d, args)
    finally:
        if args.out:
            with open(args.out, "w") as f:
                json.dump(d.results, f, indent=1, sort_keys=True)
            d.say(f"==> wrote {args.out}")
    # A finding is not a failure. The harness reads the JSON.
    return 0


if __name__ == "__main__":
    sys.exit(main())
