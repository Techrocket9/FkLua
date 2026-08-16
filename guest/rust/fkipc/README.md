# fkipc (Rust guest library)

The Rust half of FkIPC, the message-oriented link between a FkLua guest and a companion
process outside the game. It mirrors the Go guest library line for line and speaks the same
wire format. The FkIPC README, covering both guest languages, the companion SDK, the
requirements and the join-safety contract, is at
[`guest/go/fkipc/README.md`](../../go/fkipc/README.md); this crate's own package
documentation is in [`src/lib.rs`](src/lib.rs).
