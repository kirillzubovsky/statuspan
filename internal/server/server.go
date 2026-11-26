// Package server is the loopback HTTP API and the embedded status board.
package server

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/kirillzubovsky/statuspan/internal/relay"
	"github.com/kirillzubovsky/statuspan/internal/store"
)

//go:embed static/index.html
var indexHTML []byte

const maxBody = 1 << 20

// Server serves the status API and the board.
type Server struct {
	store *store.Store
	// ping is the SSE keepalive interval. Zero means 15s.
	ping time.Duration
	// relay reports sync status for /healthz. Nil means local-only.
	relay func() relay.Status
}

// New returns a server for st.
func New(st *store.Store) *Server {
	return &Server{store: st}
}

// SetRelay reports relay sync status on /healthz. Nil leaves relay unconfigured.
func (s *Server) SetRelay(fn func() relay.Status) {
	s.relay = fn
}

// Handler is the HTTP mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /favicon.ico", handleFavicon)
	mux.HandleFunc("POST /api/jobs", s.handlePost)
	mux.HandleFunc("GET /api/jobs", s.handleList)
	mux.HandleFunc("DELETE /api/jobs", s.handleClear)
	mux.HandleFunc("GET /api/jobs/{agent}/{job_id}", s.handleGet)
	mux.HandleFunc("DELETE /api/jobs/{agent}/{job_id}", s.handleDelete)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/stream", s.handleStream)
	return mux
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(indexHTML)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	st := relay.Status{}
	if s.relay != nil {
		st = s.relay()
	}
	writeJSON(w, http.StatusOK, struct {
		OK    bool         `json:"ok"`
		Relay relay.Status `json:"relay"`
	}{OK: true, Relay: st})
}

func handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "unreadable body")
		return
	}
	if len(body) > maxBody {
		writeErr(w, http.StatusBadRequest, "body too large")
		return
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 || bytes.Equal(body, []byte("null")) {
		writeErr(w, http.StatusBadRequest, "empty body")
		return
	}
	updates, err := decodeUpdates(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	stampLocal(updates)
	if err := s.store.ValidateBatch(updates); err != nil {
		writeStoreErr(w, err)
		return
	}
	applied := false
	for _, u := range updates {
		ok, err := s.store.Apply(u)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		if ok {
			applied = true
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": applied})
}

// stampLocal overwrites client provenance. Loopback HTTP and the unix socket
// both land here, and neither has a request IP worth recording.
func stampLocal(updates []store.Update) {
	for i := range updates {
		updates[i].Via = "local"
		updates[i].IP = ""
		updates[i].Country = ""
		updates[i].ReceivedAt = ""
	}
}

func decodeUpdates(body []byte) ([]store.Update, error) {
	if body[0] == '[' {
		var updates []store.Update
		if err := json.Unmarshal(body, &updates); err != nil {
			return nil, err
		}
		if updates == nil {
			return nil, fmt.Errorf("null array")
		}
		return updates, nil
	}
	var u store.Update
	if err := json.Unmarshal(body, &u); err != nil {
		return nil, err
	}
	return []store.Update{u}, nil
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	now, jobs := s.store.List()
	after, waiting := s.store.AbandonPolicySeconds()
	writeJSON(w, http.StatusOK, struct {
		Now                        time.Time       `json:"now"`
		Jobs                       []store.JobView `json:"jobs"`
		AbandonAfterSeconds        int64           `json:"abandon_after_seconds"`
		AbandonWaitingAfterSeconds int64           `json:"abandon_waiting_after_seconds"`
	}{Now: now, Jobs: jobs, AbandonAfterSeconds: after, AbandonWaitingAfterSeconds: waiting})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	job, ok := s.store.Get(r.PathValue("agent"), r.PathValue("job_id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	deleted, err := s.store.Delete(r.PathValue("agent"), r.PathValue("job_id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": deleted})
}

func (s *Server) handleClear(w http.ResponseWriter, r *http.Request) {
	confirm, err := confirmClear(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	switch confirm {
	case "all":
		n, backup, err := s.store.Clear()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": n, "backup": backup})
	case "attention", "progress", "queued", "done":
		n, err := s.store.ClearSection(confirm)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": n, "section": confirm})
	default:
		writeErr(w, http.StatusBadRequest, `confirm must be "all", "attention", "progress", "queued", or "done"`)
	}
}

func confirmClear(r *http.Request) (string, error) {
	if r.URL.Query().Has("confirm") {
		return r.URL.Query().Get("confirm"), nil
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return "", err
	}
	if len(body) > maxBody {
		return "", fmt.Errorf("body too large")
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return "", nil
	}
	var payload struct {
		Confirm string `json:"confirm"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", err
	}
	return payload.Confirm, nil
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	limit := 200
	limitSet := false
	if q := r.URL.Query().Get("limit"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = n
		limitSet = true
	}
	agent := r.URL.Query().Get("agent")
	var now time.Time
	var events []store.Event
	if agent != "" {
		now, events = s.store.AgentEvents(agent)
		if limitSet && limit < len(events) {
			events = events[:limit]
		}
	} else {
		now, events = s.store.Events(limit)
	}
	writeJSON(w, http.StatusOK, struct {
		Now    time.Time     `json:"now"`
		Events []store.Event `json:"events"`
	}{Now: now, Events: events})
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	snap, ch, cancel := s.store.Subscribe()
	defer cancel()
	if err := writeSSE(w, "snapshot", snap); err != nil {
		return
	}
	flusher.Flush()

	ping := s.ping
	if ping <= 0 {
		ping = 15 * time.Second
	}
	ticker := time.NewTicker(ping)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case push, open := <-ch:
			if !open {
				return
			}
			var err error
			if push.Snapshot != nil {
				err = writeSSE(w, "snapshot", push.Snapshot)
			} else {
				err = writeSSE(w, "job", push.Job)
			}
			if err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeSSE(w io.Writer, event string, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	data := bytes.TrimRight(buf.Bytes(), "\n")
	_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	return err
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		http.Error(w, "encode", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_, _ = w.Write(buf.Bytes())
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"ok": false, "error": msg})
}

func writeStoreErr(w http.ResponseWriter, err error) {
	var ve *store.ValidationError
	if errors.As(err, &ve) {
		writeErr(w, http.StatusBadRequest, ve.Error())
		return
	}
	writeErr(w, http.StatusInternalServerError, err.Error())
}
