// Command mockhub is a development-only stand-in for the real `kubeshark-hub`.
//
// It implements every HTTP/WebSocket endpoint that the Kubeshark CLI's
// internal/connect.Connector posts to, receives whatever the CLI sends, and
// displays it: each request is logged with structured zerolog fields and its
// (pretty-printed) body is echoed both to stdout and to any connected
// `kubeshark console` WebSocket client.
//
// Typical use — run the mock, then point the CLI at it:
//
//	go run ./devtools/mockhub --port 8898
//	kubeshark tap --set tap.proxy.hub.port=8898  # in another shell
//
// This package is a developer tool; it is intentionally not wired into the
// main `kubeshark` binary.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// hub holds the in-memory state needed to mimic the real Hub: a monotonically
// increasing script index (returned from POST /scripts) and the set of
// connected console WebSocket clients that received payloads are broadcast to.
type hub struct {
	scriptIndex int64

	mu       sync.Mutex
	clients  map[*websocket.Conn]struct{}
	upgrader websocket.Upgrader
}

func newHub() *hub {
	return &hub{
		clients: make(map[*websocket.Conn]struct{}),
		upgrader: websocket.Upgrader{
			// Dev tool: accept any origin so the CLI/browser can connect freely.
			CheckOrigin: func(*http.Request) bool { return true },
		},
	}
}

// broadcast pushes a single line to every connected console client. It mirrors
// what the real Hub streams over /scripts/logs, so `kubeshark console` shows
// the activity received by this mock in real time.
func (h *hub) broadcast(line string) {
	stamped := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), line)

	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if err := c.WriteMessage(websocket.TextMessage, []byte(stamped)); err != nil {
			// Client went away; drop it.
			_ = c.Close()
			delete(h.clients, c)
		}
	}
}

// readBody drains the request body and returns it pretty-printed when it is
// valid JSON, otherwise as-is. The raw bytes are returned alongside so callers
// can re-decode if needed.
func readBody(r *http.Request) (pretty string, raw []byte) {
	raw, _ = io.ReadAll(r.Body)
	_ = r.Body.Close()
	if len(raw) == 0 {
		return "", raw
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, raw, "", "  "); err == nil {
		return indented.String(), raw
	}
	return string(raw), raw
}

// show logs an incoming request and surfaces its body to stdout and to the
// console WebSocket clients — the "receive and display" behaviour.
func (h *hub) show(r *http.Request, what string, pretty string) {
	evt := log.Info().Str("method", r.Method).Str("path", r.URL.Path).Str("event", what)
	if pretty != "" {
		evt = evt.Int("bytes", len(pretty))
	}
	evt.Msg("Received")

	summary := what
	if pretty != "" {
		fmt.Printf("\n--- %s %s ---\n%s\n", r.Method, r.URL.Path, pretty)
		// Keep the broadcast line single-ish: collapse newlines for the console.
		summary = fmt.Sprintf("%s: %s", what, strings.Join(strings.Fields(pretty), " "))
		if len(summary) > 500 {
			summary = summary[:500] + "…"
		}
	}
	h.broadcast(summary)
}

func (h *hub) handleEcho(w http.ResponseWriter, r *http.Request) {
	// Health/reachability probe used by Connector.TestConnection and `console`.
	log.Debug().Str("path", r.URL.Path).Msg("Health probe")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("echo"))
}

func (h *hub) handleWorker(w http.ResponseWriter, r *http.Request) {
	pretty, _ := readBody(r)
	h.show(r, "worker pod", pretty)
	w.WriteHeader(http.StatusOK)
}

func (h *hub) handleRegex(w http.ResponseWriter, r *http.Request) {
	pretty, _ := readBody(r)
	h.show(r, "pod regex", pretty)
	w.WriteHeader(http.StatusOK)
}

func (h *hub) handleLicense(w http.ResponseWriter, r *http.Request) {
	pretty, _ := readBody(r)
	h.show(r, "license", pretty)
	w.WriteHeader(http.StatusOK)
}

func (h *hub) handleScriptsEnv(w http.ResponseWriter, r *http.Request) {
	pretty, _ := readBody(r)
	h.show(r, "scripting env", pretty)
	w.WriteHeader(http.StatusOK)
}

func (h *hub) handleScriptsDone(w http.ResponseWriter, r *http.Request) {
	h.show(r, "scripts done", "")
	w.WriteHeader(http.StatusOK)
}

// handleScripts serves both the collection (POST /scripts) and item
// (PUT/DELETE /scripts/{index}) routes, since net/http's default mux can't
// distinguish them on its own.
func (h *hub) handleScripts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		pretty, _ := readBody(r)
		idx := atomic.AddInt64(&h.scriptIndex, 1) - 1
		h.show(r, fmt.Sprintf("create script (index=%d)", idx), pretty)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"index": idx})
	case http.MethodPut:
		pretty, _ := readBody(r)
		h.show(r, "update script "+strings.TrimPrefix(r.URL.Path, "/scripts/"), pretty)
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		h.show(r, "delete script "+strings.TrimPrefix(r.URL.Path, "/scripts/"), "")
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleScriptsLogs upgrades to a WebSocket and streams to the client, exactly
// like the real Hub's /scripts/logs that `kubeshark console` consumes. The
// mock pushes a periodic heartbeat plus any payloads received via HTTP.
func (h *hub) handleScriptsLogs(w http.ResponseWriter, r *http.Request) {
	c, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Error().Err(err).Msg("WebSocket upgrade failed")
		return
	}

	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	log.Info().Str("remote", r.RemoteAddr).Msg("Console client connected to /scripts/logs")

	// Drop the client when it disconnects.
	defer func() {
		h.mu.Lock()
		delete(h.clients, c)
		h.mu.Unlock()
		_ = c.Close()
		log.Info().Str("remote", r.RemoteAddr).Msg("Console client disconnected")
	}()

	_ = c.WriteMessage(websocket.TextMessage, []byte("[mockhub] connected — waiting for CLI activity…"))

	// Reading is required to observe the client's close frame; we discard data.
	for {
		if _, _, err := c.ReadMessage(); err != nil {
			return
		}
	}
}

func main() {
	port := flag.Uint("port", 8898, "port to listen on (CLI default Hub port is 8898)")
	debug := flag.Bool("debug", false, "enable debug-level logging")
	flag.Parse()

	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	if *debug {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	}
	log.Logger = log.Output(zerolog.NewConsoleWriter())

	h := newHub()

	mux := http.NewServeMux()
	mux.HandleFunc("/echo", h.handleEcho)
	mux.HandleFunc("/pods/worker", h.handleWorker)
	mux.HandleFunc("/pods/regex", h.handleRegex)
	mux.HandleFunc("/license", h.handleLicense)
	mux.HandleFunc("/scripts/env", h.handleScriptsEnv)
	mux.HandleFunc("/scripts/done", h.handleScriptsDone)
	mux.HandleFunc("/scripts/logs", h.handleScriptsLogs)
	mux.HandleFunc("/scripts", h.handleScripts)  // POST (collection)
	mux.HandleFunc("/scripts/", h.handleScripts) // PUT/DELETE (item)

	// Catch-all: log anything we didn't explicitly model and return 200 so the
	// CLI's reachability checks succeed.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		pretty, _ := readBody(r)
		h.show(r, "unhandled", pretty)
		w.WriteHeader(http.StatusOK)
	})

	addr := fmt.Sprintf(":%d", *port)
	log.Info().Str("addr", addr).Msg("mockhub listening — point the CLI here (e.g. --set tap.proxy.hub.port)")
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal().Err(err).Msg("server stopped")
	}
}
