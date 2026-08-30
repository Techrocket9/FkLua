//! Builds a log line in one fixed buffer and hands it to the host without
//! allocating.
//!
//! The line-for-line mirror of `guest/go/fklog`, and that file's header carries
//! the whole argument. The short form: `fk::log(&format!(...))` allocates, and
//! under the default bump allocator a Rust guest never gives it back -- so a
//! guest that logs on a schedule grows its linear memory forever, which is in
//! every save and every multiplayer join. One downstream mod measured its entire
//! guest heap as log lines.
//!
//! OPT-IN, AND NOT WIRED INTO `fk`. A guest that logs nothing must not link a
//! buffer.
//!
//! IT DEPENDS ON `fk` AND NOTHING ELSE, deliberately: `fkapi` is generated and
//! pinned, and a hand-written library that depended on it would drag the pin
//! into every consumer that only wanted a line builder. `Value::dump` is the
//! other side of that boundary and lives in `fkapi`, writing into a buffer this
//! crate lends it through [`tail`] and [`advance`].
//!
//! ```ignore
//! fklog::start("[mymod] compiled cluster ");
//! fklog::u(root as u64);
//! fklog::s(" parts=");
//! fklog::u(n as u64);
//! fklog::end();
//! ```
//!
//! A CALL SITE MAY NOT MAKE A HOST CALL BETWEEN `start` AND `end`. There is one
//! buffer, and a synchronously-raised event whose handler logged would
//! interleave with a line that is half built.
//!
//! # Why the `unsafe` is here and is sound
//!
//! The buffer is a `static mut`, which is what "one buffer, no allocation"
//! means in a `no_std` guest. A Factorio guest is single-threaded by
//! construction -- the sandbox has no threads and the runtime dispatches one
//! call at a time -- so the aliasing rule the compiler cannot check is the one
//! the paragraph above states: a line is built in one uninterrupted run.

#![no_std]

/// The buffer's size. A line longer than it is TRUNCATED rather than grown, for
/// the reason `guest/go/fklog`'s header measures: a fixed array is a copy with
/// no reallocation path behind it, and a log line is a diagnostic that must not
/// cost every mod hundreds of kilobytes of emitted Lua.
pub const CAP: usize = 512;

static mut BUF: [u8; CAP] = [0; CAP];
static mut LEN: usize = 0;

/// Opens a line, discarding anything half-built.
pub fn start(text: &str) {
    unsafe {
        LEN = 0;
    }
    s(text);
}

/// Appends a string.
pub fn s(text: &str) {
    put(text.as_bytes());
}

/// Appends raw bytes, which is what a Lua string really is.
pub fn bytes(b: &[u8]) {
    put(b);
}

/// Appends "true" or "false".
pub fn b(v: bool) {
    s(if v { "true" } else { "false" });
}

/// Appends an unsigned integer in base 10.
pub fn u(mut v: u64) {
    let mut d = [0u8; 20];
    let mut i = d.len();
    loop {
        i -= 1;
        d[i] = b'0' + (v % 10) as u8;
        v /= 10;
        if v == 0 {
            break;
        }
    }
    put(&d[i..]);
}

/// Appends a signed integer in base 10.
pub fn i(v: i64) {
    if v < 0 {
        s("-");
        // NEGATED THROUGH i128, because -v at i64::MIN is an overflow: a debug
        // build panics on it and a release build wraps. Go defines the wrap, so
        // the two spellings agree there and only this one is defined in both --
        // which is what keeps the twins one program.
        u((v as i128).unsigned_abs() as u64);
        return;
    }
    u(v as u64);
}

/// Appends a number to one decimal place, rounded half away from zero.
///
/// One decimal and not a real float formatter, which is a large chunk of code a
/// diagnostic does not need. See the Go twin.
pub fn f1(mut v: f64) {
    if v < 0.0 {
        s("-");
        v = -v;
    }
    let mut whole = v as u64;
    let mut frac = ((v - whole as f64) * 10.0 + 0.5) as u64;
    // 9.96 rounds to 10.0 and not to 9.10.
    if frac >= 10 {
        whole += 1;
        frac -= 10;
    }
    u(whole);
    s(".");
    u(frac);
}

/// Hands the buffer to the host as a string that BORROWS it.
///
/// The host copies the bytes into a Lua string before `fk::log` returns, so the
/// borrow never outlives the call.
pub fn end() {
    unsafe {
        let n = LEN;
        let p = &raw const BUF as *const u8;
        let b = core::slice::from_raw_parts(p, n);
        // from_utf8_unchecked is not reachable from safe input here: every
        // appender above writes ASCII, and `bytes` is the one that does not --
        // so it goes through the lossy-free path a Lua string wants, which is
        // raw bytes. fk::log takes a &str, and a Lua string is bytes, so this is
        // the same reinterpretation LuaStr makes one crate over.
        fk::log(core::str::from_utf8_unchecked(b));
    }
}

/// `start` plus `end`, for a message with nothing appended to it.
pub fn line(text: &str) {
    start(text);
    end();
}

/// How many bytes the line holds.
pub fn len() -> usize {
    unsafe { LEN }
}

/// Whether the line is empty. Present because clippy asks for it beside `len`.
pub fn is_empty() -> bool {
    len() == 0
}

/// LENDS the rest of the buffer to a caller that writes into it directly;
/// [`advance`] is how that caller says how much it wrote.
///
/// This is the seam `Value::dump` uses, and it is what keeps this crate free of
/// `fkapi`: the dumper writes into a destination it was handed and returns a
/// count, and nothing about it knows where the destination came from.
///
/// ```ignore
/// fklog::start("v=");
/// fklog::advance(v.dump(fklog::tail()));
/// fklog::end();
/// ```
pub fn tail() -> &'static mut [u8] {
    unsafe {
        let n = LEN;
        let p = &raw mut BUF as *mut u8;
        core::slice::from_raw_parts_mut(p.add(n), CAP - n)
    }
}

/// Records that [`tail`]'s first `k` bytes were written. A count past the end is
/// clamped, so a dumper that miscounted truncates rather than handing the host a
/// length past the buffer.
pub fn advance(k: usize) {
    unsafe {
        LEN = core::cmp::min(LEN + k, CAP);
    }
}

fn put(src: &[u8]) {
    unsafe {
        let n = LEN;
        let room = CAP - n;
        let k = core::cmp::min(room, src.len());
        let p = &raw mut BUF as *mut u8;
        core::ptr::copy_nonoverlapping(src.as_ptr(), p.add(n), k);
        LEN = n + k;
    }
}
