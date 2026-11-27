package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTitleOverlay(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	s := openClock(t, dir, func() time.Time { return now })
	t.Cleanup(func() { _ = s.Close() })

	raw := "Please rewrite this very long and messy job title that keeps going"
	if !mustApply(t, s, Update{
		Agent:    "bot",
		JobID:    "build 1",
		State:    "running",
		Title:    strPtr(raw),
		Note:     strPtr("looking"),
		Link:     strPtr("https://example.com/job"),
		Progress: intPtr(12),
		Seq:      i64Ptr(1),
	}) {
		t.Fatal("create not applied")
	}
	before, ok := s.Get("bot", "build 1")
	if !ok {
		t.Fatal("missing job")
	}
	lines := countLines(t, dir)
	short := "Rewrite the messy title"
	v, err := s.SetTitleOverlay("bot", "build 1", "  "+short+"  ")
	if err != nil {
		t.Fatal(err)
	}
	if v.Title != short || v.RawTitle != raw {
		t.Fatalf("title=%q raw=%q", v.Title, v.RawTitle)
	}
	if !v.UpdatedAt.Equal(before.UpdatedAt) || !v.StartedAt.Equal(before.StartedAt) {
		t.Fatalf("times moved updated %s started %s", v.UpdatedAt, v.StartedAt)
	}
	if v.Seq == nil || before.Seq == nil || *v.Seq != *before.Seq || v.Progress != before.Progress || v.Note != before.Note || v.Link != before.Link || v.State != before.State {
		t.Fatalf("job fields changed: %+v", v)
	}
	if len(v.History) != len(before.History) {
		t.Fatalf("history %d want %d", len(v.History), len(before.History))
	}
	if countLines(t, dir) != lines {
		t.Fatalf("log lines %d want %d", countLines(t, dir), lines)
	}
	logged, err := os.ReadFile(filepath.Join(dir, logName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logged), short) {
		t.Fatal("overlay title written to the log")
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"raw_title":"`+raw+`"`) {
		t.Fatalf("raw_title missing: %s", encoded)
	}

	now = now.Add(time.Minute)
	if !mustApply(t, s, Update{Agent: "bot", JobID: "build 1", Title: strPtr(raw), Seq: i64Ptr(2)}) {
		t.Fatal("same title not applied")
	}
	got, ok := s.Get("bot", "build 1")
	if !ok || got.Title != short || got.RawTitle != raw || got.Seq == nil || *got.Seq != 2 || !got.UpdatedAt.Equal(now) {
		t.Fatalf("same raw dropped overlay: %+v", got)
	}

	now = now.Add(time.Minute)
	if !mustApply(t, s, Update{Agent: "bot", JobID: "build 1", Progress: intPtr(40), Seq: i64Ptr(3)}) {
		t.Fatal("heartbeat not applied")
	}
	got, ok = s.Get("bot", "build 1")
	if !ok || got.Title != short || got.RawTitle != raw || got.Progress != 40 {
		t.Fatalf("omitted title cleared overlay: %+v", got)
	}
	held := got.UpdatedAt
	if mustApply(t, s, Update{Agent: "bot", JobID: "build 1", Title: strPtr("Other older title text"), Seq: i64Ptr(2)}) {
		t.Fatal("older seq applied")
	}
	got, ok = s.Get("bot", "build 1")
	if !ok || got.Title != short || got.RawTitle != raw || !got.UpdatedAt.Equal(held) || got.Seq == nil || *got.Seq != 3 {
		t.Fatalf("older seq cleared overlay: %+v", got)
	}

	next := "Ship the release notes today"
	now = now.Add(time.Minute)
	if !mustApply(t, s, Update{Agent: "bot", JobID: "build 1", Title: strPtr(next), Seq: i64Ptr(4)}) {
		t.Fatal("new title not applied")
	}
	got, ok = s.Get("bot", "build 1")
	if !ok || got.Title != next || got.RawTitle != "" {
		t.Fatalf("new raw kept overlay: title=%q raw=%q", got.Title, got.RawTitle)
	}
	encoded, err = json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "raw_title") {
		t.Fatalf("raw_title still present: %s", encoded)
	}

	if _, err := s.Delete("bot", "build 1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("bot", "build 1"); ok {
		t.Fatal("deleted job still present")
	}

	memRaw := "Remember this very long title that should stay after restart"
	memShort := "Remember the long title"
	if !mustApply(t, s, Update{Agent: "bot", JobID: "mem", State: "running", Title: strPtr(memRaw)}) {
		t.Fatal("mem not applied")
	}
	if _, err := s.SetTitleOverlay("bot", "mem", memShort); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	re, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = re.Close() })
	got, ok = re.Get("bot", "mem")
	if !ok || got.Title != memRaw || got.RawTitle != "" {
		t.Fatalf("restart kept overlay: title=%q raw=%q ok=%v", got.Title, got.RawTitle, ok)
	}
	logged, err = os.ReadFile(filepath.Join(dir, logName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logged), memShort) || strings.Contains(string(logged), short) {
		t.Fatal("overlay title survived in the log")
	}

	_, err = re.SetTitleOverlay("missing", "job", "Hello there now")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing job: %v", err)
	}
	_, err = re.SetTitleOverlay("bot", "mem", "   ")
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Error() != "title is required" {
		t.Fatalf("blank title: %v", err)
	}
}
