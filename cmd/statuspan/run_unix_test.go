//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunSignalExit(t *testing.T) {
	setNonTTYStdin(t)
	ts, rec := newPostServer(t)
	err := run([]string{
		"run", "--url", ts.URL + "/api/jobs",
		"--agent", "t", "--job", "j",
		"--", "sh", "-c", "kill -TERM $$",
	})
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 128+int(syscall.SIGTERM) {
		t.Fatalf("%v", err)
	}
	hits := rec.snapshot()
	if len(hits) == 0 {
		t.Fatal("no posts")
	}
	last := hits[len(hits)-1].body
	note, _ := last["note"].(string)
	if last["state"] != "failed" || !strings.Contains(note, "Exited with code 143") {
		t.Fatalf("last %+v", last)
	}
}

func TestRunForwardsSIGTERM(t *testing.T) {
	setNonTTYStdin(t)
	code, note := signalSleepingChild(t, syscall.SIGTERM)
	if code != 128+int(syscall.SIGTERM) {
		t.Fatalf("code %d note %q", code, note)
	}
	if !strings.Contains(note, "Exited with code 143") {
		t.Fatalf("note %q", note)
	}
}

func TestRunForwardsSIGINTWhenStdinIsNotATTY(t *testing.T) {
	setNonTTYStdin(t)
	code, note := signalSleepingChild(t, syscall.SIGINT)
	want := 128 + int(syscall.SIGINT)
	if code != want {
		t.Fatalf("code %d note %q", code, note)
	}
	if !strings.Contains(note, "Exited with code "+strconv.Itoa(want)) {
		t.Fatalf("note %q", note)
	}
}

func TestRunDoesNotForwardSIGINTWhenStdinIsCharDevice(t *testing.T) {
	dev, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dev.Close() })
	setStdin(t, dev)
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		t.Fatal("stdin is not a char device")
	}

	ts, rec := newPostServer(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	ready := filepath.Join(dir, "ready")
	pgidFile := filepath.Join(dir, "pgid")
	t.Cleanup(func() { killPIDFile(pidFile) })
	script := "ps -o pgid= -p $$ > " + shellQuote(pgidFile) +
		"; echo $$ > " + shellQuote(pidFile) +
		"; echo x > " + shellQuote(ready) +
		"; exec sleep 30"
	errCh := make(chan error, 1)
	go func() {
		errCh <- run([]string{
			"run", "--url", ts.URL + "/api/jobs",
			"--agent", "t", "--job", "sig",
			"--", "sh", "-c", script,
		})
	}()
	waitForFile(t, ready)
	assertChildProcessGroup(t, pgidFile)

	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		t.Fatalf("SIGINT was forwarded: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err = waitRun(t, errCh)
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 128+int(syscall.SIGTERM) {
		t.Fatalf("%v", err)
	}
	note := lastNote(t, rec.snapshot())
	if !strings.Contains(note, "Exited with code 143") {
		t.Fatalf("note %q", note)
	}
}

func signalSleepingChild(t *testing.T, sig syscall.Signal) (int, string) {
	t.Helper()
	ts, rec := newPostServer(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	ready := filepath.Join(dir, "ready")
	pgidFile := filepath.Join(dir, "pgid")
	t.Cleanup(func() { killPIDFile(pidFile) })
	script := "ps -o pgid= -p $$ > " + shellQuote(pgidFile) +
		"; echo $$ > " + shellQuote(pidFile) +
		"; echo x > " + shellQuote(ready) +
		"; exec sleep 30"
	errCh := make(chan error, 1)
	go func() {
		errCh <- run([]string{
			"run", "--url", ts.URL + "/api/jobs",
			"--agent", "t", "--job", "sig",
			"--", "sh", "-c", script,
		})
	}()
	waitForFile(t, ready)
	assertChildProcessGroup(t, pgidFile)
	if err := syscall.Kill(os.Getpid(), sig); err != nil {
		t.Fatal(err)
	}
	err := waitRun(t, errCh)
	var ee *exitError
	if !errors.As(err, &ee) {
		t.Fatalf("%v", err)
	}
	return ee.code, lastNote(t, rec.snapshot())
}

func assertChildProcessGroup(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("pgid %q", b)
	}
	if got != syscall.Getpgrp() {
		t.Fatalf("child pgid %d parent %d", got, syscall.Getpgrp())
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", filepath.Base(path))
}

func waitRun(t *testing.T, errCh <-chan error) error {
	t.Helper()
	select {
	case err := <-errCh:
		return err
	case <-time.After(4 * time.Second):
		t.Fatal("timed out waiting for run")
		return nil
	}
}

func lastNote(t *testing.T, hits []posted) string {
	t.Helper()
	if len(hits) == 0 {
		t.Fatal("no posts")
	}
	last := hits[len(hits)-1].body
	note, _ := last["note"].(string)
	if last["state"] != "failed" {
		t.Fatalf("last %+v", last)
	}
	return note
}

func setStdin(t *testing.T, f *os.File) {
	t.Helper()
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = old })
}

func setNonTTYStdin(t *testing.T) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		r.Close()
		w.Close()
	})
	setStdin(t, r)
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice != 0 {
		t.Fatal("stdin is still a char device")
	}
}

func shellQuote(path string) string {
	return "'" + strings.ReplaceAll(path, "'", "'\\''") + "'"
}

func killPIDFile(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 || pid == os.Getpid() {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
