package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// The page is EMBEDDED, with no external stylesheet, font or script, for the
// same reason a published artifact is: this runs on a machine that may have no
// network and it must not depend on a CDN being reachable to show a slider.
//
//go:embed static
var static embed.FS

// hub is the SSE fan-out.
//
// A CHANNEL PER SUBSCRIBER WITH A NON-BLOCKING SEND, because the alternative --
// blocking on a slow browser -- would stall the telemetry callback, which runs
// on the session's own receive goroutine. A dropped update is invisible: the
// next frame carries the whole state, so this is a state stream and not a log,
// which is the same reasoning that makes an fkipc channel drop a stale frame
// rather than reorder it.
type hub struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

func newHub() *hub { return &hub{subs: map[chan struct{}]struct{}{}} }

func (h *hub) subscribe() (chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

func (h *hub) notify() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func serve(lg *slog.Logger, h *hub, links []*link, addr string, gamePort uint16) {
	byKey := map[string]*link{}
	var list []modSpec
	for _, l := range links {
		byKey[l.spec.Key] = l
		list = append(list, l.spec)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		b, err := static.ReadFile("static/index.html")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})

	mux.HandleFunc("/api/spec", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"gamePort": gamePort, "mods": list})
	})

	// SSE, chosen over a websocket because the traffic is one-directional
	// state: the browser needs a stream in and ordinary POSTs out, and SSE is
	// that with no handshake, no framing library and automatic reconnect.
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		ch, cancel := h.subscribe()
		defer cancel()
		// A floor on the update rate so "the session went quiet" is visible as
		// an ageing readout rather than as a page that simply stopped.
		tick := time.NewTicker(time.Second)
		defer tick.Stop()

		send := func() bool {
			states := make([]modState, 0, len(links))
			for _, l := range links {
				states = append(states, l.state())
			}
			b, err := json.Marshal(states)
			if err != nil {
				return false
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return false
			}
			flusher.Flush()
			return true
		}
		if !send() {
			return
		}
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ch:
			case <-tick.C:
			}
			if !send() {
				return
			}
		}
	})

	// One slider move. The browser throttles to ~10/s; this does not queue or
	// coalesce, because a set is idempotent and the LAST one wins by
	// construction -- an intermediate value that loses a race is a value the
	// slider passed through anyway.
	mux.HandleFunc("POST /api/set", func(w http.ResponseWriter, r *http.Request) {
		l := byKey[r.URL.Query().Get("mod")]
		if l == nil {
			http.Error(w, "unknown mod", 400)
			return
		}
		key := r.URL.Query().Get("key")
		value, err := strconv.Atoi(r.URL.Query().Get("value"))
		if err != nil {
			http.Error(w, "value must be an integer", 400)
			return
		}
		if !allowed(l.spec, key) {
			http.Error(w, "unknown key", 400)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		ack, err := l.set(ctx, key, value)
		if err != nil {
			// A failed set is REPORTED rather than retried. The library will
			// not retry across a session boundary by itself, and only the
			// application knows whether re-asking is safe; here the person is
			// the application and the next slider move is the retry.
			writeJSON(w, map[string]any{"ack": "", "err": err.Error()})
			return
		}
		h.notify()
		writeJSON(w, map[string]any{"ack": ack, "err": ""})
	})

	lg.Info("ipcdemo serving", "url", "http://localhost"+portOf(addr),
		"game_port", gamePort)
	for _, l := range links {
		lg.Info("mod", "key", l.spec.Key, "listen", l.spec.Port, "lang", l.spec.Lang)
	}

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			lg.Error("http", "err", err)
		}
	}()
	waitForSignal()
	lg.Info("stopping")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}

func allowed(s modSpec, key string) bool {
	for _, sl := range s.Sliders {
		if sl.Key == key {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func portOf(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i:]
		}
	}
	return ":" + addr
}
