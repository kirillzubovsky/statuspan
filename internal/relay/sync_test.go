package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kirillzubovsky/statuspan/internal/store"
)

func TestMain(m *testing.M) {
	os.Unsetenv("STATUSPAN_RELAY_URL")
	os.Unsetenv("STATUSPAN_RELAY_WORKSPACE")
	os.Unsetenv("STATUSPAN_RELAY_READ_TOKEN")
	os.Unsetenv("STATUSPAN_TOKEN")
	os.Exit(m.Run())
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLocalOnlyDoesNotStartSync(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	client := &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		t.Errorf("local-only mode requested %s", r.URL.String())
		return nil, fmt.Errorf("unexpected request")
	})}
	syncer, err := New(Options{Store: st, DataDir: dir, Client: client})
	if err != nil {
		t.Fatal(err)
	}
	if syncer != nil {
		t.Fatal("local-only mode started sync")
	}
	if _, err := os.Stat(filepath.Join(dir, cursorFile)); !os.IsNotExist(err) {
		t.Fatal("cursor file was created")
	}
}

func TestResolvePrecedence(t *testing.T) {
	dir := t.TempDir()
	fileBody := `{"url":"http://file.example","workspace":"fromfile","read_token":"file-token-value"}`
	if err := os.WriteFile(filepath.Join(dir, configFile), []byte(fileBody), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, ok, err := Resolve(Input{
		DataDir:      dir,
		EnvURL:       "http://env.example",
		EnvWorkspace: "fromenv",
		EnvToken:     "env-token-value",
	})
	if err != nil || !ok {
		t.Fatalf("env resolve ok=%v err=%v", ok, err)
	}
	if cfg.URL != "http://env.example" || cfg.Workspace != "fromenv" || cfg.Token != "env-token-value" {
		t.Fatalf("%+v", cfg)
	}

	tokPath := filepath.Join(dir, "read-token")
	if err := os.WriteFile(tokPath, []byte("flag-token-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, ok, err = Resolve(Input{
		DataDir:       dir,
		FlagURL:       "http://flag.example/base/",
		FlagWorkspace: "Flagged",
		FlagTokenFile: tokPath,
		EnvURL:        "http://env.example",
		EnvWorkspace:  "fromenv",
		EnvToken:      "env-token-value",
	})
	if err != nil || !ok {
		t.Fatalf("flag resolve ok=%v err=%v", ok, err)
	}
	if cfg.URL != "http://flag.example/base" || cfg.Workspace != "Flagged" || cfg.Token != "flag-token-value" {
		t.Fatalf("%+v", cfg)
	}

	if _, ok, err := Resolve(Input{DataDir: t.TempDir()}); err != nil || ok {
		t.Fatalf("empty ok=%v err=%v", ok, err)
	}
	if _, _, err := Resolve(Input{FlagURL: "http://flag.example"}); err == nil {
		t.Fatal("partial config should fail")
	}
	badDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(badDir, configFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Resolve(Input{DataDir: badDir}); err == nil {
		t.Fatal("invalid relay.json should fail")
	}
}

func TestNewUsesEnvOverFile(t *testing.T) {
	t.Setenv("STATUSPAN_RELAY_URL", "http://env.example")
	t.Setenv("STATUSPAN_RELAY_WORKSPACE", "envws")
	t.Setenv("STATUSPAN_RELAY_READ_TOKEN", "env-token-value")
	dir := t.TempDir()
	fileBody := `{"url":"http://file.example","workspace":"fromfile","read_token":"file-token-value"}`
	if err := os.WriteFile(filepath.Join(dir, configFile), []byte(fileBody), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	syncer, err := New(Options{Store: st, DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if syncer == nil || syncer.BaseURL() != "http://env.example" || syncer.Workspace() != "envws" {
		t.Fatalf("syncer=%v", syncer)
	}
}

func TestSyncMergeCursorResumeAndDuplicates(t *testing.T) {
	token := "read-token-do-not-leak"
	ts1 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	ts2 := ts1.Add(time.Minute)
	ts3 := ts2.Add(time.Minute)
	fake := &fakeRelay{token: token}
	title := "Build"
	link := "http://example.test/a"
	seq1 := int64(1)
	prog1 := 10
	fake.events = append(fake.events, relayEvent{
		Cursor: 1, N: 1, Agent: "Bot", JobID: "j", State: "running",
		Title: &title, Link: &link, Progress: &prog1, Seq: &seq1, TS: ts1, Kind: "update",
	})
	seq2 := int64(2)
	prog2 := 40
	note := "halfway"
	fake.events = append(fake.events, relayEvent{
		Cursor: 2, N: 2, Agent: "Bot", JobID: "j",
		Progress: &prog2, Note: &note, Seq: &seq2, TS: ts2, Kind: "update",
	})

	ts := httptest.NewServer(fake)
	defer ts.Close()
	dir := t.TempDir()
	body := fmt.Sprintf(`{"url":%q,"workspace":"home","read_token":%q}`, ts.URL, token)
	if err := os.WriteFile(filepath.Join(dir, configFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	syncer, err := New(Options{Store: st, DataDir: dir, Client: ts.Client()})
	if err != nil || syncer == nil {
		t.Fatalf("syncer=%v err=%v", syncer, err)
	}
	_, ch, cancel := st.Subscribe()
	defer cancel()
	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case push := <-ch:
		job := push.Job
		if job.Agent != "Bot" || job.JobID != "j" || job.Title != "Build" {
			t.Fatalf("sse job %+v", job)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber did not receive the merged job")
	}

	job, ok := st.Get("Bot", "j")
	if !ok {
		t.Fatal("job missing")
	}
	if job.Title != "Build" || job.Progress != 40 || job.Note != "halfway" || job.State != "running" || job.Link != link {
		t.Fatalf("merged %+v", job)
	}
	if job.Seq == nil || *job.Seq != 2 {
		t.Fatalf("seq %v", job.Seq)
	}
	if !job.UpdatedAt.Equal(ts2) {
		t.Fatalf("updated_at %s", job.UpdatedAt)
	}
	if got := strings.TrimSpace(readFile(t, filepath.Join(dir, cursorFile))); got != "2" {
		t.Fatalf("cursor file %q", got)
	}
	fake.mu.Lock()
	if len(fake.since) != 1 || fake.since[0] != "0" || fake.path != "/v1/w/home/events" || fake.auth != "Bearer "+token {
		t.Fatalf("request since=%v path=%s auth=%s", fake.since, fake.path, fake.auth)
	}
	fake.mu.Unlock()

	seq3 := int64(3)
	prog3 := 55
	fake.add(relayEvent{
		Cursor: 3, N: 3, Agent: "Bot", JobID: "j", Progress: &prog3, Seq: &seq3, TS: ts3, Kind: "update",
	})
	resumed, err := New(Options{Store: st, DataDir: dir, Client: ts.Client()})
	if err != nil || resumed == nil {
		t.Fatalf("resume syncer=%v err=%v", resumed, err)
	}
	if err := resumed.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	if fake.since[len(fake.since)-1] != "2" {
		t.Fatalf("resume since %v", fake.since)
	}
	fake.mu.Unlock()
	job, _ = st.Get("Bot", "j")
	if job.Progress != 55 || job.Seq == nil || *job.Seq != 3 || job.Title != "Build" {
		t.Fatalf("after resume %+v", job)
	}
	if got := strings.TrimSpace(readFile(t, filepath.Join(dir, cursorFile))); got != "3" {
		t.Fatalf("cursor file %q", got)
	}

	localSeq := int64(10)
	localTitle := "local-wins"
	localProg := 90
	if _, err := st.Apply(store.Update{
		Agent: "Bot", JobID: "j", State: "running", Seq: &localSeq, Title: &localTitle, Progress: &localProg,
	}); err != nil {
		t.Fatal(err)
	}
	oldSeq := int64(4)
	oldTitle := "old"
	oldProg := 1
	evil := "http://evil.example/nope"
	fake.add(relayEvent{
		Cursor: 4, N: 4, Agent: "Bot", JobID: "j", State: "failed",
		Title: &oldTitle, Progress: &oldProg, Link: &evil, Seq: &oldSeq, TS: ts3.Add(time.Minute), Kind: "update",
	})
	if err := resumed.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	job, _ = st.Get("Bot", "j")
	if job.Title != "local-wins" || job.Progress != 90 || job.State != "running" || job.Link != link {
		t.Fatalf("older relay overwrote local %+v", job)
	}
	if job.Seq == nil || *job.Seq != 10 {
		t.Fatalf("seq %v", job.Seq)
	}
	sawOld := false
	for _, ev := range job.History {
		if ev.Seq != nil && *ev.Seq == 4 {
			sawOld = true
			if ev.Applied {
				t.Fatal("older seq was applied")
			}
		}
	}
	if !sawOld {
		t.Fatal("older seq missing from history")
	}

	hist := len(job.History)
	lines := countLogLines(t, dir)
	fake.mu.Lock()
	fake.ignoreSince = true
	fake.mu.Unlock()
	if err := resumed.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	fresh, err := New(Options{Store: st, DataDir: dir, Client: ts.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	job, _ = st.Get("Bot", "j")
	if len(job.History) != hist || countLogLines(t, dir) != lines || job.Title != "local-wins" {
		t.Fatalf("duplicate fetch changed state hist=%d lines=%d job=%+v", len(job.History), countLogLines(t, dir), job)
	}
	status := fresh.Status()
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), token) {
		t.Fatalf("status leaked token: %s", raw)
	}
	if !status.Configured || status.Cursor != 4 || status.LastOK == nil || status.LastError != "" {
		t.Fatalf("status %+v", status)
	}
}

func TestDuplicateNoSeqIsHarmless(t *testing.T) {
	token := "read-token-do-not-leak"
	fake := &fakeRelay{token: token, ignoreSince: true}
	title := "Once"
	when := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	fake.events = []relayEvent{{
		Cursor: 1, N: 1, Agent: "Bot", JobID: "once", State: "running", Title: &title, TS: when, Kind: "update",
	}}
	ts := httptest.NewServer(fake)
	defer ts.Close()
	dir := t.TempDir()
	writeRelayJSON(t, dir, ts.URL, "home", token)
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	syncer, err := New(Options{Store: st, DataDir: dir, Client: ts.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	job, ok := st.Get("Bot", "once")
	if !ok || job.Title != "Once" || len(job.History) != 1 {
		t.Fatalf("dup %+v ok=%v", job, ok)
	}
}

func TestSyncPagesUntilCaughtUp(t *testing.T) {
	token := "read-token-do-not-leak"
	fake := &fakeRelay{token: token}
	when := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for i := int64(1); i <= 3; i++ {
		seq := i
		title := fmt.Sprintf("t%d", i)
		fake.events = append(fake.events, relayEvent{
			Cursor: i, N: i, Agent: "Bot", JobID: "p", State: "running",
			Title: &title, Seq: &seq, TS: when.Add(time.Duration(i) * time.Second), Kind: "update",
		})
	}
	ts := httptest.NewServer(fake)
	defer ts.Close()
	dir := t.TempDir()
	writeRelayJSON(t, dir, ts.URL, "home", token)
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	syncer, err := New(Options{Store: st, DataDir: dir, Client: ts.Client(), PageLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	got := append([]string(nil), fake.since...)
	fake.mu.Unlock()
	if strings.Join(got, ",") != "0,1,2,3" {
		t.Fatalf("since %v", got)
	}
	job, ok := st.Get("Bot", "p")
	if !ok || job.Title != "t3" || job.Seq == nil || *job.Seq != 3 {
		t.Fatalf("%+v ok=%v", job, ok)
	}
}

func TestBackoffRedactsToken(t *testing.T) {
	token := "read-token-do-not-leak"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("auth %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/v1/w/home/events" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"ok":false,"error":"boom %s"}`, token)
	}))
	defer ts.Close()
	dir := t.TempDir()
	writeRelayJSON(t, dir, ts.URL, "home", token)
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var logs []string
	var waits []time.Duration
	syncer, err := New(Options{
		Store:   st,
		DataDir: dir,
		Client:  ts.Client(),
		Logf: func(format string, args ...any) {
			logs = append(logs, fmt.Sprintf(format, args...))
		},
		Wait: func(ctx context.Context, d time.Duration) error {
			waits = append(waits, d)
			if len(waits) == 2 {
				return context.Canceled
			}
			return nil
		},
		Jitter: func(d time.Duration) time.Duration { return d },
	})
	if err != nil {
		t.Fatal(err)
	}
	syncer.Run(context.Background())
	if len(waits) != 2 || waits[0] != time.Second || waits[1] != 2*time.Second {
		t.Fatalf("waits %v", waits)
	}
	blob := strings.Join(logs, "\n")
	if strings.Contains(blob, token) || !strings.Contains(blob, "boom [redacted]") || !strings.Contains(blob, "500") {
		t.Fatalf("log %q", blob)
	}
	raw, err := json.Marshal(syncer.Status())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), token) {
		t.Fatalf("status %s", raw)
	}
	if syncer.Status().LastError == "" {
		t.Fatal("missing last error")
	}
}

func TestClearSectionStaysClearedAcrossSync(t *testing.T) {
	token := "read-token-do-not-leak"
	when := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	runTitle := "Working"
	doneTitle := "Shipped"
	seq := int64(1)
	fake := &fakeRelay{token: token}
	fake.events = append(fake.events,
		relayEvent{
			Cursor: 1, N: 1, Agent: "Bot", JobID: "run", State: "running",
			Title: &runTitle, Seq: &seq, TS: when, Kind: "update",
		},
		relayEvent{
			Cursor: 2, N: 2, Agent: "Bot", JobID: "ship", State: "done",
			Title: &doneTitle, Seq: &seq, TS: when.Add(time.Second), Kind: "update",
		},
	)
	ts := httptest.NewServer(fake)
	defer ts.Close()
	dir := t.TempDir()
	writeRelayJSON(t, dir, ts.URL, "home", token)
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	syncer, err := New(Options{Store: st, DataDir: dir, Client: ts.Client()})
	if err != nil || syncer == nil {
		t.Fatalf("syncer=%v err=%v", syncer, err)
	}
	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if job, ok := st.Get("Bot", "run"); !ok || job.Title != "Working" || job.State != "running" {
		t.Fatalf("run %+v ok=%v", job, ok)
	}
	if job, ok := st.Get("Bot", "ship"); !ok || job.State != "done" {
		t.Fatalf("ship %+v ok=%v", job, ok)
	}
	n, err := st.ClearSection("done")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("deleted %d", n)
	}
	if _, ok := st.Get("Bot", "ship"); ok {
		t.Fatal("done job survived clear")
	}
	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Get("Bot", "ship"); ok {
		t.Fatal("sync reapplied the cleared job")
	}
	if job, ok := st.Get("Bot", "run"); !ok || job.Title != "Working" || job.State != "running" {
		t.Fatalf("run after resync %+v ok=%v", job, ok)
	}
	back := "Again"
	seq2 := int64(2)
	fake.add(relayEvent{
		Cursor: 3, N: 3, Agent: "Bot", JobID: "ship", State: "running",
		Title: &back, Seq: &seq2, TS: when.Add(time.Minute), Kind: "update",
	})
	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	job, ok := st.Get("Bot", "ship")
	if !ok || job.Title != "Again" || job.State != "running" {
		t.Fatalf("revived %+v ok=%v", job, ok)
	}
	if kept, ok := st.Get("Bot", "run"); !ok || kept.Title != "Working" {
		t.Fatalf("run after revive %+v ok=%v", kept, ok)
	}
}

func TestPartialRelayJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, configFile), []byte(`{"url":"http://127.0.0.1:9"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{DataDir: dir}); err == nil {
		t.Fatal("partial relay.json should fail")
	}
}

type fakeRelay struct {
	token       string
	mu          sync.Mutex
	events      []relayEvent
	ignoreSince bool
	since       []string
	path        string
	auth        string
}

func (f *fakeRelay) add(ev relayEvent) {
	f.mu.Lock()
	f.events = append(f.events, ev)
	f.mu.Unlock()
}

func (f *fakeRelay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.since = append(f.since, r.URL.Query().Get("since"))
	f.path = r.URL.Path
	f.auth = r.Header.Get("Authorization")
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"error":"unauthorized"}`))
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	var chosen []relayEvent
	for _, ev := range f.events {
		if !f.ignoreSince && ev.id() <= since {
			continue
		}
		chosen = append(chosen, ev)
		if limit > 0 && len(chosen) >= limit {
			break
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"events": chosen, "next_cursor": since})
}

func writeRelayJSON(t *testing.T, dir, base, workspace, token string) {
	t.Helper()
	body := fmt.Sprintf(`{"url":%q,"workspace":%q,"read_token":%q}`, base, workspace, token)
	if err := os.WriteFile(filepath.Join(dir, configFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func countLogLines(t *testing.T, dir string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			n++
		}
	}
	return n
}
