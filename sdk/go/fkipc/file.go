package fkipc

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/Techrocket9/fklua/guest/go/fkipc/wire"
)

// File pickup.
//
// NOTHING DOCUMENTS A FLUSH GUARANTEE for helpers.write_file, so a notify is
// not "the bytes are all there" and this side must decide when it is. There are
// two cases and they get different tests, which is the whole reason
// HAS_DIGEST exists:
//
//   - GUEST-WRITTEN. The guest held the bytes, so it knows the length and the
//     FNV-1a-32 over them. The test is EXACT: read until Bytes and the checksum
//     matches, or keep waiting.
//   - ENGINE-WRITTEN (a screenshot). The guest has never held the bytes and
//     cannot describe them. No digest, so the fallback is stabilize-polling:
//     the size unchanged across two polls.
//
// The second is a heuristic and is labelled as one. It is what is available.

const (
	pickupPoll    = 40 * time.Millisecond
	pickupTimeout = 20 * time.Second
)

// ErrPickupTimeout is what OnFile logs when a notify never became a readable
// file. The handler is not called.
var ErrPickupTimeout = errors.New("fkipc: the notified file never satisfied its notify")

func pickUp(dir string, n FileNotify, h func(FileNotify, io.ReadCloser), log *slog.Logger) {
	if dir == "" {
		d, err := DefaultScriptOutput()
		if err != nil {
			log.Warn("fkipc: no script-output directory", "err", err)
			return
		}
		dir = d
	}
	path := filepath.Join(dir, filepath.FromSlash(n.Name))
	deadline := time.Now().Add(pickupTimeout)
	var lastSize int64 = -1

	for time.Now().Before(deadline) {
		st, err := os.Stat(path)
		if err == nil && !st.IsDir() {
			if n.HasDigest {
				if st.Size() == int64(n.Bytes) {
					b, err := os.ReadFile(path)
					if err == nil && len(b) == int(n.Bytes) &&
						wire.FNV1a32(b) == n.Digest {
						h(n, io.NopCloser(newByteReader(b)))
						return
					}
				}
			} else if st.Size() == lastSize && st.Size() > 0 {
				f, err := os.Open(path)
				if err == nil {
					h(n, f)
					return
				}
			}
			lastSize = st.Size()
		}
		time.Sleep(pickupPoll)
	}
	log.Warn("fkipc: file pickup timed out", "name", n.Name, "path", path,
		"digest", n.HasDigest)
}

type byteReader struct {
	b []byte
	i int
}

func newByteReader(b []byte) *byteReader { return &byteReader{b: b} }

func (r *byteReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

// DefaultScriptOutput returns the platform's Factorio script-output directory.
//
// IT IS A FALLBACK, NOT A GUESS THE SDK MAKES FOR YOU. scripts/run-probe.sh
// guesses three ways and that is right for a harness that runs on one machine;
// an SDK a downstream author points at their own install must TAKE the
// directory, and Options.ScriptOutput is where. This is what happens when they
// did not say.
//
// FACTORIO_USERDIR wins, because that is the variable this repo's own in-game
// harnesses set -- a headless run under a private user directory writes its
// script-output there and nowhere near the default.
func DefaultScriptOutput() (string, error) {
	if d := os.Getenv("FACTORIO_USERDIR"); d != "" {
		return filepath.Join(d, "script-output"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "factorio",
			"script-output"), nil
	case "windows":
		if ad := os.Getenv("APPDATA"); ad != "" {
			return filepath.Join(ad, "Factorio", "script-output"), nil
		}
		return filepath.Join(home, "AppData", "Roaming", "Factorio",
			"script-output"), nil
	default:
		return filepath.Join(home, ".factorio", "script-output"), nil
	}
}
