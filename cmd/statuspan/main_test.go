package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kirillzubovsky/statuspan/internal/server"
	"github.com/kirillzubovsky/statuspan/internal/store"
)

func TestMain(m *testing.M) {
	os.Unsetenv("STATUSPAN_RELAY_URL")
	os.Unsetenv("STATUSPAN_RELAY_WORKSPACE")
	os.Unsetenv("STATUSPAN_RELAY_READ_TOKEN")
	os.Unsetenv("STATUSPAN_TOKEN")
	os.Unsetenv("STATUSPAN_ABANDON_AFTER")
	os.Unsetenv("STATUSPAN_ABANDON_WAITING_AFTER")
	os.Exit(m.Run())
}

func TestPostAllowsOmittedState(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ts := httptest.NewServer(server.New(st).Handler())
	t.Cleanup(ts.Close)

	if err := postCmd([]string{"--addr", ts.URL, "--agent", "My Bot", "--job", "build-1", "--state", "running", "--title", "Build", "--progress", "10"}); err != nil {
		t.Fatal(err)
	}
	if err := postCmd([]string{"--addr", ts.URL, "--agent", "My Bot", "--job", "build-1", "--progress", "40", "--note", "halfway"}); err != nil {
		t.Fatal(err)
	}
	job, ok := st.Get("My Bot", "build-1")
	if !ok || job.State != "running" || job.Progress != 40 || job.Note != "halfway" || job.Title != "Build" {
		t.Fatalf("%+v ok=%v", job, ok)
	}
	if err := postCmd([]string{"--addr", ts.URL, "--agent", "My Bot", "--job", "new", "--title", "no state"}); err == nil {
		t.Fatal("new job without state should fail")
	}
	if _, ok := st.Get("My Bot", "new"); ok {
		t.Fatal("new job was created")
	}
	if err := postCmd([]string{"--job", "j", "--state", "running"}); err == nil {
		t.Fatal("missing agent should fail")
	}
}

func TestPostRelaySendsBearerAndPath(t *testing.T) {
	token := "write-token-secret"
	type hit struct {
		auth   string
		path   string
		method string
		body   map[string]any
	}
	var hits []hit
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("body: %v", err)
		}
		hits = append(hits, hit{
			auth:   r.Header.Get("Authorization"),
			path:   r.URL.Path,
			method: r.Method,
			body:   body,
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"applied":true}`))
	}))
	defer ts.Close()

	err := postCmd([]string{
		"--relay", ts.URL,
		"--workspace", "home",
		"--token", token,
		"--agent", "MyBot",
		"--job", "build-1",
		"--state", "running",
		"--title", "Build v1",
		"--progress", "40",
	})
	if err != nil {
		t.Fatal(err)
	}
	tokPath := filepath.Join(t.TempDir(), "write-token")
	if err := os.WriteFile(tokPath, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := postCmd([]string{
		"--relay", ts.URL,
		"--workspace", "home",
		"--token-file", tokPath,
		"--agent", "MyBot",
		"--job", "build-1",
		"--progress", "55",
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STATUSPAN_RELAY_URL", ts.URL)
	t.Setenv("STATUSPAN_RELAY_WORKSPACE", "home")
	t.Setenv("STATUSPAN_TOKEN", token)
	if err := postCmd([]string{"--agent", "MyBot", "--job", "build-1", "--state", "running"}); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Fatalf("hits %d", len(hits))
	}
	for i, got := range hits {
		if got.method != http.MethodPost {
			t.Fatalf("hit %d method %s", i, got.method)
		}
		if got.auth != "Bearer "+token {
			t.Fatalf("hit %d auth %q", i, got.auth)
		}
		if got.path != "/v1/w/home/jobs" {
			t.Fatalf("hit %d path %q", i, got.path)
		}
		if got.body["agent"] != "MyBot" || got.body["job_id"] != "build-1" {
			t.Fatalf("hit %d body %v", i, got.body)
		}
	}
	if hits[0].body["state"] != "running" || hits[0].body["title"] != "Build v1" {
		t.Fatalf("first body %v", hits[0].body)
	}
}

func TestPostRelayPrintsStatusAndMessage(t *testing.T) {
	token := "write-token-secret"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"ok":false,"error":"token is scoped to a different agent"}`))
	}))
	defer ts.Close()
	err := postCmd([]string{
		"--relay", ts.URL,
		"--workspace", "home",
		"--token", token,
		"--agent", "MyBot",
		"--job", "build-1",
		"--state", "running",
	})
	if err == nil {
		t.Fatal("expected relay error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "403") || !strings.Contains(msg, "token is scoped to a different agent") {
		t.Fatal(msg)
	}
	if strings.Contains(msg, token) {
		t.Fatal(msg)
	}
}

func TestClearCommandRequiresYesAndPrintsBackup(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Apply(store.Update{Agent: "bot", JobID: "j", State: "running", Title: strPtr("Keep me")}); err != nil {
		t.Fatal(err)
	}
	hit := 0
	guard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit++
		http.Error(w, "production must not be cleared", http.StatusInternalServerError)
	}))
	t.Cleanup(guard.Close)

	err = clearCmd([]string{"--addr", guard.URL})
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("missing --yes: %v", err)
	}
	if hit != 0 {
		t.Fatal("clear without --yes called the server")
	}
	if _, ok := st.Get("bot", "j"); !ok {
		t.Fatal("job removed without --yes")
	}

	ts := httptest.NewServer(server.New(st).Handler())
	t.Cleanup(ts.Close)
	var cmdErr error
	withStdout(t, func() {
		cmdErr = clearCmd([]string{"--addr", ts.URL, "--yes"})
	})
	out := lastStdout
	if cmdErr != nil {
		t.Fatal(cmdErr)
	}
	if _, ok := st.Get("bot", "j"); ok {
		t.Fatal("job survived clear --yes")
	}
	if _, ev := st.Events(20); len(ev) != 0 {
		t.Fatalf("events left %d", len(ev))
	}
	if !strings.Contains(out, "deleted 1\n") {
		t.Fatalf("stdout %q", out)
	}
	backups, err := filepath.Glob(filepath.Join(dir, "jobs-backup-*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 {
		t.Fatalf("backups %v", backups)
	}
	raw, err := os.ReadFile(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "Keep me") {
		t.Fatalf("backup %s", raw)
	}
	if !strings.Contains(out, backups[0]) {
		t.Fatalf("stdout missing backup path %q\n%s", backups[0], out)
	}
}

var lastStdout string

func withStdout(t *testing.T, fn func()) error {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	fn()
	os.Stdout = old
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	lastStdout = buf.String()
	return nil
}

func strPtr(s string) *string { return &s }

func TestParseNonNegativeDuration(t *testing.T) {
	d, err := parseNonNegativeDuration("abandon-after", "24h")
	if err != nil || d != 24*time.Hour {
		t.Fatalf("%v %v", d, err)
	}
	d, err = parseNonNegativeDuration("abandon-waiting-after", " 168h ")
	if err != nil || d != 168*time.Hour {
		t.Fatalf("%v %v", d, err)
	}
	d, err = parseNonNegativeDuration("abandon-after", "0")
	if err != nil || d != 0 {
		t.Fatalf("%v %v", d, err)
	}
	if _, err := parseNonNegativeDuration("abandon-after", "-1h"); err == nil {
		t.Fatal("accepted a negative duration")
	}
	if _, err := parseNonNegativeDuration("abandon-after", "nope"); err == nil {
		t.Fatal("accepted an invalid duration")
	}
}

func TestServeRejectsBadAbandonDuration(t *testing.T) {
	err := serveCmd([]string{"--addr", "127.0.0.1:0", "--data", t.TempDir(), "--abandon-after", "nope"})
	if err == nil || !strings.Contains(err.Error(), "abandon-after") {
		t.Fatal(err)
	}
	err = serveCmd([]string{"--addr", "127.0.0.1:0", "--data", t.TempDir(), "--abandon-waiting-after", "-1h"})
	if err == nil || !strings.Contains(err.Error(), ">=") {
		t.Fatal(err)
	}
}
