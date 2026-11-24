package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type posted struct {
	auth string
	path string
	body map[string]any
}

type recorder struct {
	mu   sync.Mutex
	hits []posted
}

func (r *recorder) snapshot() []posted {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]posted, len(r.hits))
	copy(out, r.hits)
	return out
}

func (r *recorder) reset() {
	r.mu.Lock()
	r.hits = nil
	r.mu.Unlock()
}

func newPostServer(t *testing.T) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("body: %v", err)
		}
		rec.mu.Lock()
		rec.hits = append(rec.hits, posted{auth: r.Header.Get("Authorization"), path: r.URL.Path, body: body})
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"applied":true}`))
	}))
	t.Cleanup(ts.Close)
	return ts, rec
}

func TestRunSuccessHeartbeatsAndDone(t *testing.T) {
	ts, rec := newPostServer(t)
	t.Setenv("STATUSPAN_TEST_HELPER", "sleep:2s")
	err := run([]string{
		"run", "--url", ts.URL + "/api/jobs",
		"--agent", "My Agent", "--job", "build-1", "--title", "Build v1",
		"--heartbeat", "1", "--ttl", "600",
		"--", os.Args[0],
	})
	if err != nil {
		t.Fatal(err)
	}
	hits := rec.snapshot()
	if len(hits) < 3 {
		t.Fatalf("hits %d %+v", len(hits), hits)
	}
	if hits[0].body["state"] != "running" || hits[0].body["progress"] != float64(0) {
		t.Fatalf("first %+v", hits[0].body)
	}
	running := 0
	var prev int64 = -1
	for i, hit := range hits {
		if hit.body["state"] == "running" {
			running++
		}
		seq, ok := hit.body["seq"].(float64)
		if !ok {
			t.Fatalf("hit %d seq %v", i, hit.body["seq"])
		}
		if int64(seq) <= prev {
			t.Fatalf("seq not increasing at %d: %v", i, seq)
		}
		prev = int64(seq)
		if hit.path != "/api/jobs" {
			t.Fatalf("path %s", hit.path)
		}
	}
	if running < 2 {
		t.Fatalf("running posts %d", running)
	}
	last := hits[len(hits)-1].body
	note, _ := last["note"].(string)
	if last["state"] != "done" || last["progress"] != float64(100) || !strings.Contains(note, "Completed in ") {
		t.Fatalf("last %+v", last)
	}
}

func TestRunExitCodePostsFailed(t *testing.T) {
	ts, rec := newPostServer(t)
	t.Setenv("STATUSPAN_TEST_HELPER", "exit:42")
	err := run([]string{
		"run", "--url", ts.URL + "/api/jobs",
		"--agent", "t", "--job", "j",
		"--", os.Args[0],
	})
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 42 {
		t.Fatalf("%v", err)
	}
	hits := rec.snapshot()
	if len(hits) < 2 {
		t.Fatalf("hits %+v", hits)
	}
	if hits[0].body["state"] != "running" {
		t.Fatalf("first %+v", hits[0].body)
	}
	last := hits[len(hits)-1].body
	note, _ := last["note"].(string)
	if last["state"] != "failed" || !strings.Contains(note, "Exited with code 42") {
		t.Fatalf("last %+v", last)
	}
	assertIncreasingSeq(t, hits)
}

func TestRunUnreachableKeepsExit(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STATUSPAN_TEST_HELPER", "exit:7")
	start := time.Now()
	err = run([]string{
		"run", "--url", "http://" + addr + "/api/jobs",
		"--agent", "t", "--job", "j",
		"--", os.Args[0],
	})
	if time.Since(start) > 4*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 7 {
		t.Fatalf("%v", err)
	}
}

func TestRunRecurring(t *testing.T) {
	ts, rec := newPostServer(t)
	t.Setenv("STATUSPAN_TEST_HELPER", "exit:0")
	err := run([]string{
		"run", "--recurring", "--url", ts.URL + "/api/jobs",
		"--agent", "t", "--job", "digest",
		"--", os.Args[0],
	})
	if err != nil {
		t.Fatal(err)
	}
	if hits := rec.snapshot(); len(hits) != 0 {
		t.Fatalf("success posts %+v", hits)
	}

	rec.reset()
	t.Setenv("STATUSPAN_TEST_HELPER", "exit:3")
	err = run([]string{
		"run", "--recurring", "--url", ts.URL + "/api/jobs",
		"--agent", "t", "--job", "digest",
		"--", os.Args[0],
	})
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 3 {
		t.Fatalf("%v", err)
	}
	hits := rec.snapshot()
	if len(hits) != 1 {
		t.Fatalf("hits %+v", hits)
	}
	note, _ := hits[0].body["note"].(string)
	if hits[0].body["state"] != "failed" || !strings.Contains(note, "Exited with code 3") {
		t.Fatalf("%+v", hits[0].body)
	}
}

func TestRunMissingBinary(t *testing.T) {
	ts, rec := newPostServer(t)
	err := run([]string{
		"run", "--url", ts.URL + "/api/jobs",
		"--agent", "t", "--job", "j",
		"--", "statuspan-missing-bin-xyz",
	})
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 127 {
		t.Fatalf("%v", err)
	}
	if !hasFailedStart(rec.snapshot()) {
		t.Fatalf("hits %+v", rec.snapshot())
	}

	rec.reset()
	err = run([]string{
		"run", "--recurring", "--url", ts.URL + "/api/jobs",
		"--agent", "t", "--job", "j",
		"--", "statuspan-missing-bin-xyz",
	})
	if !errors.As(err, &ee) || ee.code != 127 {
		t.Fatalf("%v", err)
	}
	hits := rec.snapshot()
	if len(hits) != 1 || !hasFailedStart(hits) {
		t.Fatalf("recurring hits %+v", hits)
	}
}

func TestRunRelayBearerTokenFile(t *testing.T) {
	const token = "write-token-secret"
	ts, rec := newPostServer(t)
	tokPath := filepath.Join(t.TempDir(), "write-token")
	if err := os.WriteFile(tokPath, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STATUSPAN_TEST_HELPER", "exit:0")
	err := run([]string{
		"run", "--relay", ts.URL, "--workspace", "home", "--token-file", tokPath,
		"--agent", "My Agent", "--job", "build-1",
		"--", os.Args[0],
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("STATUSPAN_RELAY_URL", ts.URL)
	t.Setenv("STATUSPAN_RELAY_WORKSPACE", "home")
	t.Setenv("STATUSPAN_TOKEN_FILE", tokPath)
	err = run([]string{
		"run", "--agent", "My Agent", "--job", "build-1",
		"--", os.Args[0],
	})
	if err != nil {
		t.Fatal(err)
	}
	hits := rec.snapshot()
	if len(hits) < 2 {
		t.Fatalf("hits %d", len(hits))
	}
	for i, hit := range hits {
		if hit.auth != "Bearer "+token {
			t.Fatalf("hit %d auth %q", i, hit.auth)
		}
		if hit.path != "/v1/w/home/jobs" {
			t.Fatalf("hit %d path %q", i, hit.path)
		}
		if strings.Contains(hit.auth, "\n") {
			t.Fatal("token kept a newline")
		}
	}
	assertIncreasingSeq(t, hits)
}

func TestRunReportMisconfigStillRuns(t *testing.T) {
	ts, rec := newPostServer(t)
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("STATUSPAN_ADDR", u.Host)
	t.Setenv("STATUSPAN_RELAY_URL", "")
	t.Setenv("STATUSPAN_RELAY_WORKSPACE", "")
	t.Setenv("STATUSPAN_TOKEN", "")
	t.Setenv("STATUSPAN_TOKEN_FILE", "")
	const secret = "super-secret-token"

	check := func(t *testing.T, args []string, warning string) {
		t.Helper()
		rec.reset()
		t.Setenv("STATUSPAN_TEST_HELPER", "exit:3")
		stderr, err := captureStderr(t, func() error {
			return run(args)
		})
		var ee *exitError
		if !errors.As(err, &ee) || ee.code != 3 {
			t.Fatalf("err %v stderr %q", err, stderr)
		}
		if strings.Count(stderr, "\n") != 1 || !strings.HasPrefix(stderr, "statuspan: warning:") {
			t.Fatalf("stderr %q", stderr)
		}
		if warning != "" && !strings.Contains(stderr, warning) {
			t.Fatalf("stderr %q", stderr)
		}
		if strings.Contains(stderr, secret) {
			t.Fatalf("token leaked: %q", stderr)
		}
		if hits := rec.snapshot(); len(hits) != 0 {
			t.Fatalf("posts %+v stderr %q", hits, stderr)
		}
	}

	t.Run("incomplete relay env", func(t *testing.T) {
		t.Setenv("STATUSPAN_RELAY_URL", ts.URL)
		check(t, []string{
			"run", "--agent", "t", "--job", "j",
			"--", os.Args[0],
		}, "missing")
	})

	t.Run("missing token file", func(t *testing.T) {
		t.Setenv("STATUSPAN_RELAY_URL", ts.URL)
		t.Setenv("STATUSPAN_RELAY_WORKSPACE", "home")
		t.Setenv("STATUSPAN_TOKEN_FILE", filepath.Join(t.TempDir(), "missing-token"))
		check(t, []string{
			"run", "--agent", "t", "--job", "j",
			"--", os.Args[0],
		}, "token file")
	})

	t.Run("missing token file flag", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "missing-token")
		check(t, []string{
			"run", "--relay", ts.URL, "--workspace", "home", "--token-file", missing,
			"--agent", "t", "--job", "j",
			"--", os.Args[0],
		}, "token file")
	})

	t.Run("token is not printed", func(t *testing.T) {
		t.Setenv("STATUSPAN_RELAY_URL", ts.URL)
		t.Setenv("STATUSPAN_TOKEN", secret)
		check(t, []string{
			"run", "--agent", "t", "--job", "j",
			"--", os.Args[0],
		}, "missing")
	})

	t.Run("bad url", func(t *testing.T) {
		check(t, []string{
			"run", "--url", "not-a-url",
			"--agent", "t", "--job", "j",
			"--", os.Args[0],
		}, "--url")
	})

	t.Run("bad addr", func(t *testing.T) {
		check(t, []string{
			"run", "--addr", "not-a-port",
			"--agent", "t", "--job", "j",
			"--", os.Args[0],
		}, "--addr")
	})

	t.Run("url combined with relay", func(t *testing.T) {
		t.Setenv("STATUSPAN_TOKEN", secret)
		check(t, []string{
			"run", "--relay", ts.URL, "--workspace", "home", "--url", ts.URL + "/api/jobs",
			"--agent", "t", "--job", "j",
			"--", os.Args[0],
		}, "cannot be combined")
	})
}

func TestRunUsageDoesNotStartCommand(t *testing.T) {
	ts, rec := newPostServer(t)
	t.Setenv("STATUSPAN_TEST_HELPER", "exit:5")
	cases := [][]string{
		{"run", "--agent", "t", "--job", "j"},
		{"run", "--url", ts.URL + "/api/jobs", "--agent", "t", "--job", "j", "--ttl", "-1", "--", os.Args[0]},
		{"run", "--url", ts.URL + "/api/jobs", "--agent", "t", "--job", "j", "--heartbeat", "0", "--", os.Args[0]},
		{"run", "--url", ts.URL + "/api/jobs", "--agent", "t", "--job", "j", "--no-such", "--", os.Args[0]},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			rec.reset()
			_, err := captureStderr(t, func() error { return run(args) })
			var ee *exitError
			if !errors.As(err, &ee) || ee.code != 2 {
				t.Fatalf("%v", err)
			}
			if hits := rec.snapshot(); len(hits) != 0 {
				t.Fatalf("posts %+v", hits)
			}
		})
	}
}

func captureStderr(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	runErr := fn()
	if err := w.Close(); err != nil {
		os.Stderr = old
		t.Fatal(err)
	}
	os.Stderr = old
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	return buf.String(), runErr
}

func TestRunRequiresCommand(t *testing.T) {
	err := run([]string{"run", "--agent", "t", "--job", "j"})
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 {
		t.Fatalf("%v", err)
	}
}

func TestDefaultHeartbeat(t *testing.T) {
	if defaultHeartbeat(600) != 60*time.Second {
		t.Fatal(defaultHeartbeat(600))
	}
	if defaultHeartbeat(30) != 15*time.Second {
		t.Fatal(defaultHeartbeat(30))
	}
	if defaultHeartbeat(10) != 10*time.Second {
		t.Fatal(defaultHeartbeat(10))
	}
	if defaultHeartbeat(0) != 10*time.Second {
		t.Fatal(defaultHeartbeat(0))
	}
}

func hasFailedStart(hits []posted) bool {
	for _, hit := range hits {
		note, _ := hit.body["note"].(string)
		if hit.body["state"] == "failed" && strings.Contains(note, "Could not start:") {
			return true
		}
	}
	return false
}

func assertIncreasingSeq(t *testing.T, hits []posted) {
	t.Helper()
	var prev int64 = -1
	for i, hit := range hits {
		seq, ok := hit.body["seq"].(float64)
		if !ok || int64(seq) <= prev {
			t.Fatalf("seq at %d: %v", i, hit.body["seq"])
		}
		prev = int64(seq)
	}
}
