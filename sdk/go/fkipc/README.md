# fkipc (companion SDK)

The companion-process half of FkIPC: a Go module for a program that runs beside Factorio and talks to a mod built with FkLua. `Dial`, `Subscribe`, `Request`, `OnFile`. The FkIPC README, covering the guest libraries, the requirements and the join-safety contract, is at [`guest/go/fkipc/README.md`](../../../guest/go/fkipc/README.md); this package's own documentation is in [`doc.go`](doc.go), and [`../cmd/ipcgate`](../cmd/ipcgate/main.go) is the worked consumer.
