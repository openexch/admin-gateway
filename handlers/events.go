// SPDX-License-Identifier: Apache-2.0
package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Tunables for the SSE stream; vars so tests can shrink them.
var (
	eventsHeartbeat    = 15 * time.Second
	eventsProgressPoll = 250 * time.Millisecond
)

// handleEvents streams agent lifecycle events and operation progress as
// Server-Sent Events — the first consumer of the ProcessAgent Subscribe
// stream. Two named event types share the stream:
//
//	event: process  — one agent.Event (started/stopped/crashed/cascade-stop/
//	                  disarmed/adopted), JSON verbatim
//	event: progress — the /api/admin/progress map, emitted on connect and
//	                  whenever it changes (poll-based; replaces the UI's
//	                  50ms HTTP fast-poll during operations)
//
// Delivery is best-effort BY DESIGN: Subscribe's bounded buffer drops events
// for a slow consumer rather than wedging the crash path, and there is no
// Last-Event-ID replay — the stream is live-only. Clients re-seed from
// /api/admin/status after (re)connecting; /status stays the source of truth.
func (h *Handlers) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}

	events, unsub := h.procMgr.Subscribe(64)
	defer unsub()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	writeEvent := func(name string, v interface{}) {
		data, err := json.Marshal(v)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
		fl.Flush()
	}

	// Initial progress snapshot so a client connecting mid-operation renders
	// the bar immediately.
	lastSig := progressSignature(h.progress.ToMap())
	writeEvent("progress", h.progress.ToMap())

	hb := time.NewTicker(eventsHeartbeat)
	defer hb.Stop()
	prog := time.NewTicker(eventsProgressPoll)
	defer prog.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			writeEvent("process", ev)
		case <-prog.C:
			m := h.progress.ToMap()
			if sig := progressSignature(m); !bytes.Equal(sig, lastSig) {
				lastSig = sig
				writeEvent("progress", m)
			}
		case <-hb.C:
			// Comment frame: keeps proxies and EventSource from timing out.
			fmt.Fprint(w, ": hb\n\n")
			fl.Flush()
		}
	}
}

// progressSignature is the change-detection key for progress frames:
// everything except elapsedMs, which advances every read and would otherwise
// re-emit an unchanged operation four times a second.
func progressSignature(m map[string]interface{}) []byte {
	sig := make(map[string]interface{}, len(m))
	for k, v := range m {
		if k == "elapsedMs" {
			continue
		}
		sig[k] = v
	}
	b, _ := json.Marshal(sig)
	return b
}
