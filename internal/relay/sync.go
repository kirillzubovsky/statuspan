package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kirillzubovsky/statuspan/internal/store"
)

const (
	pollInterval  = 3 * time.Second
	maxBackoff    = 60 * time.Second
	maxEventLimit = 1000
	maxPages      = 1000
	maxBody       = 8 << 20
)

// Options configures a syncer. Leave the zero values for production defaults.
type Options struct {
	Store         *store.Store
	DataDir       string
	FlagURL       string
	FlagWorkspace string
	FlagTokenFile string
	Client        *http.Client
	Interval      time.Duration
	PageLimit     int
	Logf          func(string, ...any)
	// Wait, when set, replaces the sleep between polls. Tests use it.
	Wait func(context.Context, time.Duration) error
	// Jitter, when set, replaces backoff jitter. Tests use it.
	Jitter func(time.Duration) time.Duration
}

// Syncer pulls relay events into the local store.
// A nil *Syncer means relay sync is not configured.
type Syncer struct {
	store     *store.Store
	cfg       Config
	dir       string
	client    *http.Client
	interval  time.Duration
	pageLimit int
	log       func(string, ...any)
	wait      func(context.Context, time.Duration) error
	jitter    func(time.Duration) time.Duration

	mu      sync.Mutex
	cursor  int64
	seen    map[int64]struct{}
	lastOK  time.Time
	lastErr string
}

// New resolves config and, when relay sync is configured, returns a syncer.
// It returns nil, nil in local-only mode. The caller must not start a loop then.
func New(opt Options) (*Syncer, error) {
	cfg, ok, err := Resolve(Input{
		DataDir:       opt.DataDir,
		FlagURL:       opt.FlagURL,
		FlagWorkspace: opt.FlagWorkspace,
		FlagTokenFile: opt.FlagTokenFile,
		EnvURL:        os.Getenv("STATUSPAN_RELAY_URL"),
		EnvWorkspace:  os.Getenv("STATUSPAN_RELAY_WORKSPACE"),
		EnvToken:      os.Getenv("STATUSPAN_RELAY_READ_TOKEN"),
	})
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	if opt.Store == nil {
		return nil, fmt.Errorf("relay: nil store")
	}
	cur, err := loadCursor(opt.DataDir)
	if err != nil {
		return nil, err
	}
	client := opt.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	limit := opt.PageLimit
	if limit <= 0 || limit > maxEventLimit {
		limit = maxEventLimit
	}
	interval := opt.Interval
	if interval <= 0 {
		interval = pollInterval
	}
	logf := opt.Logf
	if logf == nil {
		logf = log.Printf
	}
	return &Syncer{
		store:     opt.Store,
		cfg:       cfg,
		dir:       opt.DataDir,
		client:    client,
		interval:  interval,
		pageLimit: limit,
		log:       logf,
		wait:      opt.Wait,
		jitter:    opt.Jitter,
		cursor:    cur,
		seen:      make(map[int64]struct{}),
	}, nil
}

// BaseURL is the relay origin without the token.
func (s *Syncer) BaseURL() string {
	if s == nil {
		return ""
	}
	return s.cfg.URL
}

// Workspace is the relay workspace name.
func (s *Syncer) Workspace() string {
	if s == nil {
		return ""
	}
	return s.cfg.Workspace
}

// Status is a snapshot for /healthz. It does not include the token.
func (s *Syncer) Status() Status {
	if s == nil {
		return Status{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{
		Configured: true,
		Cursor:     s.cursor,
		LastError:  s.lastErr,
	}
	if !s.lastOK.IsZero() {
		t := s.lastOK
		st.LastOK = &t
	}
	return st
}

// Run polls until ctx is cancelled. The first poll happens immediately.
func (s *Syncer) Run(ctx context.Context) {
	if s == nil {
		return
	}
	backoff := time.Duration(0)
	for {
		if ctx.Err() != nil {
			return
		}
		err := s.SyncOnce(ctx)
		wait := s.interval
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.logf("statuspan relay: %s", err.Error())
			backoff = nextBackoff(backoff)
			wait = s.jitterDur(backoff)
		} else {
			backoff = 0
		}
		if err := s.sleep(ctx, wait); err != nil {
			return
		}
	}
}

// SyncOnce fetches events until caught up and merges them into the store.
func (s *Syncer) SyncOnce(ctx context.Context) error {
	if s == nil {
		return nil
	}
	err := s.sync(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		msg := s.redact(err.Error())
		s.mu.Lock()
		s.lastErr = msg
		s.mu.Unlock()
		return errors.New(msg)
	}
	s.mu.Lock()
	s.lastOK = time.Now().UTC()
	s.lastErr = ""
	s.mu.Unlock()
	return nil
}

func (s *Syncer) sync(ctx context.Context) error {
	since := s.snapshotCursor()
	for page := 0; page < maxPages; page++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		events, err := s.fetch(ctx, since)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}
		advanced := false
		for _, ev := range events {
			next, err := s.consume(ev)
			if err != nil {
				return err
			}
			if next > since {
				since = next
				advanced = true
			}
		}
		if !advanced || len(events) < s.pageLimit {
			return nil
		}
	}
	return nil
}

func (s *Syncer) fetch(ctx context.Context, since int64) ([]relayEvent, error) {
	endpoint, err := eventsURL(s.cfg, since, s.pageLimit)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s: %s", req.URL.Path, resp.Status, ErrorText(body))
	}
	var page struct {
		Events []relayEvent `json:"events"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("GET %s: invalid json", req.URL.Path)
	}
	return page.Events, nil
}

// consume merges one relay event. A repeated cursor is a no-op.
// The returned cursor is the event id when the event was handled.
func (s *Syncer) consume(ev relayEvent) (int64, error) {
	id := ev.id()
	if id <= 0 {
		return 0, fmt.Errorf("relay event missing cursor")
	}
	s.mu.Lock()
	_, dup := s.seen[id]
	cur := s.cursor
	s.mu.Unlock()
	if dup || id <= cur {
		s.markSeen(id)
		if id > cur {
			if err := s.advanceCursor(id); err != nil {
				return cur, err
			}
		}
		return id, nil
	}
	if ev.Kind != "" && ev.Kind != "update" {
		s.logf("statuspan relay: skip cursor %d kind %s", id, s.redact(ev.Kind))
		s.markSeen(id)
		if err := s.advanceCursor(id); err != nil {
			return cur, err
		}
		return id, nil
	}
	if _, err := s.store.ApplyAt(ev.toUpdate(), ev.TS); err != nil {
		var ve *store.ValidationError
		if !errors.As(err, &ve) {
			return cur, err
		}
		s.logf("statuspan relay: skip cursor %d: %s", id, s.redact(ve.Error()))
	}
	s.markSeen(id)
	if err := s.advanceCursor(id); err != nil {
		return cur, err
	}
	return id, nil
}

func (s *Syncer) advanceCursor(n int64) error {
	if err := saveCursor(s.dir, n); err != nil {
		return err
	}
	s.mu.Lock()
	if n > s.cursor {
		s.cursor = n
	}
	s.mu.Unlock()
	return nil
}

func (s *Syncer) markSeen(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = make(map[int64]struct{})
	}
	s.seen[id] = struct{}{}
	if len(s.seen) <= 10000 {
		return
	}
	for k := range s.seen {
		if k < s.cursor {
			delete(s.seen, k)
		}
	}
}

func (s *Syncer) snapshotCursor() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor
}

func (s *Syncer) redact(msg string) string {
	if s.cfg.Token != "" && len(s.cfg.Token) >= 8 {
		msg = strings.ReplaceAll(msg, s.cfg.Token, "[redacted]")
	}
	msg = oneLine(msg)
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return msg
}

func (s *Syncer) logf(format string, args ...any) {
	if s.log == nil {
		return
	}
	s.log(format, args...)
}

func (s *Syncer) sleep(ctx context.Context, d time.Duration) error {
	if s.wait != nil {
		return s.wait(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *Syncer) jitterDur(d time.Duration) time.Duration {
	if s.jitter != nil {
		return s.jitter(d)
	}
	return jitter(d)
}

func nextBackoff(cur time.Duration) time.Duration {
	if cur <= 0 {
		return time.Second
	}
	cur *= 2
	if cur > maxBackoff {
		return maxBackoff
	}
	return cur
}

func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	span := int64(d)/2 + 1
	return d/2 + time.Duration(rand.Int64N(span))
}

func eventsURL(cfg Config, since int64, limit int) (string, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return "", err
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/w/" + url.PathEscape(cfg.Workspace) + "/events"
	q := url.Values{}
	q.Set("since", strconv.FormatInt(since, 10))
	q.Set("limit", strconv.Itoa(limit))
	u.RawQuery = q.Encode()
	u.Fragment = ""
	return u.String(), nil
}

func loadCursor(dir string) (int64, error) {
	b, err := os.ReadFile(filepath.Join(dir, cursorFile))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	text := strings.TrimSpace(string(b))
	if text == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("relay-cursor: invalid")
	}
	return n, nil
}

func saveCursor(dir string, n int64) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, cursorFile)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(strconv.FormatInt(n, 10) + "\n")
	serr := f.Sync()
	cerr := f.Close()
	if werr != nil || serr != nil || cerr != nil {
		os.Remove(tmp)
		if werr != nil {
			return werr
		}
		if serr != nil {
			return serr
		}
		return cerr
	}
	return os.Rename(tmp, path)
}

type relayEvent struct {
	Cursor     int64     `json:"cursor"`
	N          int64     `json:"n"`
	Agent      string    `json:"agent"`
	JobID      string    `json:"job_id"`
	State      string    `json:"state"`
	Progress   *int      `json:"progress"`
	Note       *string   `json:"note"`
	Title      *string   `json:"title"`
	Link       *string   `json:"link"`
	TS         time.Time `json:"ts"`
	Seq        *int64    `json:"seq"`
	Kind       string    `json:"kind"`
	Via        string    `json:"via,omitempty"`
	IP         string    `json:"ip,omitempty"`
	Country    string    `json:"country,omitempty"`
	ReceivedAt string    `json:"received_at,omitempty"`
}

func (ev relayEvent) id() int64 {
	if ev.Cursor != 0 {
		return ev.Cursor
	}
	return ev.N
}

func (ev relayEvent) toUpdate() store.Update {
	u := store.Update{
		Agent:      ev.Agent,
		JobID:      ev.JobID,
		State:      ev.State,
		Progress:   ev.Progress,
		Seq:        ev.Seq,
		Via:        ev.Via,
		IP:         ev.IP,
		Country:    ev.Country,
		ReceivedAt: ev.ReceivedAt,
	}
	if ev.Title != nil {
		title := *ev.Title
		u.Title = &title
	}
	if ev.Note != nil {
		note := *ev.Note
		u.Note = &note
	}
	if ev.Link != nil {
		link := *ev.Link
		u.Link = &link
	}
	return u
}
