package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func strPtr(s string) *string { return &s }
func intPtr(n int) *int       { return &n }
func i64Ptr(n int64) *int64   { return &n }

func openClock(t *testing.T, dir string, now func() time.Time) *Store {
	t.Helper()
	s, err := OpenWith(dir, Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustApply(t *testing.T, s *Store, u Update) bool {
	t.Helper()
	ok, err := s.Apply(u)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	return ok
}

func dumpJobs(t *testing.T, s *Store) string {
	t.Helper()
	_, jobs := s.List()
	b, err := json.Marshal(jobs)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func countLines(t *testing.T, dir string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, logName))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

func TestValidate(t *testing.T) {
	base := Update{Agent: "a", JobID: "j", State: "running"}
	if err := Validate(base); err != nil {
		t.Fatal(err)
	}
	cases := []Update{
		{JobID: "j", State: "running"},
		{Agent: " ", JobID: "j", State: "running"},
		{Agent: "a/b", JobID: "j", State: "running"},
		{Agent: "a", State: "running"},
		{Agent: "a", JobID: "j\\x", State: "running"},
		{Agent: "a", JobID: "j", State: "Running"},
		{Agent: "a", JobID: "j", State: "running", Progress: intPtr(101)},
		{Agent: "a", JobID: "j", State: "running", Progress: intPtr(-1)},
		{Agent: "a", JobID: "j", State: "running", TTL: intPtr(-1)},
		{Agent: "a", JobID: "j", State: "abandoned"},
	}
	for _, u := range cases {
		if err := Validate(u); err == nil {
			t.Fatalf("expected error for %+v", u)
		}
	}
	if err := Validate(Update{Agent: "a", JobID: "j", State: "done", Progress: intPtr(0)}); err != nil {
		t.Fatal(err)
	}
	if err := Validate(Update{Agent: "a", JobID: "j", State: "failed", Progress: intPtr(100)}); err != nil {
		t.Fatal(err)
	}
	// Empty state is decided once the store knows whether the job exists.
	if err := Validate(Update{Agent: "a", JobID: "j"}); err != nil {
		t.Fatal(err)
	}
}

func TestSeqOrderingAndMerge(t *testing.T) {
	dir := t.TempDir()
	cur := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	s := openClock(t, dir, func() time.Time { return cur })
	defer s.Close()

	if !mustApply(t, s, Update{
		Agent: "bot", JobID: "j", State: "running", Seq: i64Ptr(5),
		Title: strPtr("A"), Progress: intPtr(10), Link: strPtr("http://example.test/a"), TTL: intPtr(600),
	}) {
		t.Fatal("first apply")
	}

	if mustApply(t, s, Update{
		Agent: "bot", JobID: "j", State: "failed", Seq: i64Ptr(5),
		Title: strPtr("ignored"), Progress: intPtr(99),
	}) {
		t.Fatal("equal seq should be ignored")
	}
	if mustApply(t, s, Update{
		Agent: "bot", JobID: "j", State: "failed", Seq: i64Ptr(4), Title: strPtr("older"),
	}) {
		t.Fatal("older seq should be ignored")
	}

	job, ok := s.Get("bot", "j")
	if !ok {
		t.Fatal("missing job")
	}
	if job.Title != "A" || job.Progress != 10 || job.State != "running" || job.Link != "http://example.test/a" {
		t.Fatalf("state changed on ignored seq: %+v", job)
	}
	if len(job.History) != 3 || job.History[1].Applied || job.History[2].Applied {
		t.Fatalf("history = %+v", job.History)
	}

	// A missing seq is newer than any stored seq, and omitted fields keep previous values.
	if !mustApply(t, s, Update{Agent: "bot", JobID: "j", State: "blocked", Note: strPtr("waiting")}) {
		t.Fatal("missing seq should apply")
	}
	job, _ = s.Get("bot", "j")
	if job.Title != "A" || job.Progress != 10 || job.Link != "http://example.test/a" || job.Note != "waiting" || job.State != "blocked" {
		t.Fatalf("merge failed: %+v", job)
	}
	if job.Seq == nil || *job.Seq != 5 {
		t.Fatalf("stored seq = %v", job.Seq)
	}
	if job.TTL != 600 {
		t.Fatalf("ttl = %d", job.TTL)
	}

	// The stored seq is still 5, so seq 5 is still not newer.
	if mustApply(t, s, Update{Agent: "bot", JobID: "j", State: "running", Seq: i64Ptr(5), Title: strPtr("nope")}) {
		t.Fatal("seq 5 should still be ignored")
	}
	if !mustApply(t, s, Update{Agent: "bot", JobID: "j", State: "running", Seq: i64Ptr(6), Progress: intPtr(0)}) {
		t.Fatal("higher seq should apply")
	}
	job, _ = s.Get("bot", "j")
	if job.Title != "A" || job.Progress != 0 || job.Note != "waiting" || job.State != "running" {
		t.Fatalf("after seq 6: %+v", job)
	}

	// Explicit zero progress must stick; a later omitted progress must not restore it.
	if !mustApply(t, s, Update{Agent: "bot", JobID: "j", State: "running", Title: strPtr("B")}) {
		t.Fatal("second missing seq")
	}
	job, _ = s.Get("bot", "j")
	if job.Progress != 0 || job.Title != "B" {
		t.Fatalf("omitted progress changed: %+v", job)
	}
}

func TestStalledAndElapsed(t *testing.T) {
	dir := t.TempDir()
	cur := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	s := openClock(t, dir, func() time.Time { return cur })
	defer s.Close()

	ttl := 10
	mustApply(t, s, Update{Agent: "bot", JobID: "run", State: "running", TTL: &ttl, Title: strPtr("work")})
	_, jobs := s.List()
	if jobs[0].Stalled || jobs[0].ElapsedSeconds != 0 {
		t.Fatalf("fresh job: %+v", jobs[0])
	}

	cur = cur.Add(10 * time.Second)
	job, _ := s.Get("bot", "run")
	if job.Stalled {
		t.Fatal("age == ttl is not stalled")
	}
	if job.ElapsedSeconds != 10 {
		t.Fatalf("elapsed = %d", job.ElapsedSeconds)
	}

	cur = cur.Add(time.Millisecond)
	job, _ = s.Get("bot", "run")
	if !job.Stalled {
		t.Fatal("expected stalled just after ttl")
	}

	doneAt := cur
	mustApply(t, s, Update{Agent: "bot", JobID: "run", State: "done"})
	cur = cur.Add(time.Hour)
	job, _ = s.Get("bot", "run")
	if job.Stalled {
		t.Fatal("done jobs are not stalled")
	}
	if job.ElapsedSeconds != 10 {
		t.Fatalf("finished elapsed = %d, want 10", job.ElapsedSeconds)
	}
	if job.FinishedAt == nil || !job.FinishedAt.Equal(doneAt) {
		t.Fatalf("finished_at = %v", job.FinishedAt)
	}

	// Default ttl is 600s. Only running stalls. Queued never does.
	start := cur
	mustApply(t, s, Update{Agent: "bot", JobID: "q", State: "queued"})
	cur = start.Add(601 * time.Second)
	job, _ = s.Get("bot", "q")
	if job.Stalled || job.TTL != defaultTTL {
		t.Fatalf("queued job: stalled=%v ttl=%d", job.Stalled, job.TTL)
	}

	mustApply(t, s, Update{Agent: "bot", JobID: "later", State: "running"})
	cur = cur.Add(600 * time.Second)
	job, _ = s.Get("bot", "later")
	if job.Stalled || job.TTL != defaultTTL {
		t.Fatalf("default ttl running at boundary: stalled=%v ttl=%d", job.Stalled, job.TTL)
	}
	cur = cur.Add(time.Second)
	job, _ = s.Get("bot", "later")
	if !job.Stalled {
		t.Fatal("running job should stall after 600s")
	}
	job, _ = s.Get("bot", "q")
	if job.Stalled {
		t.Fatal("queued job should not stall")
	}

	mustApply(t, s, Update{Agent: "bot", JobID: "later", State: "blocked"})
	job, _ = s.Get("bot", "later")
	if job.Stalled {
		t.Fatal("blocked job is not stalled")
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	cur := time.Date(2026, 6, 2, 8, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return cur }
	s := openClock(t, dir, nowFn)

	mustApply(t, s, Update{
		Agent: "bot", JobID: "j", State: "running", Seq: i64Ptr(3),
		Title: strPtr("Build"), Progress: intPtr(40), Link: strPtr("http://example.test/x?a=1&b=2"),
		Note: strPtr("first"), TTL: intPtr(30),
	})
	cur = cur.Add(2 * time.Second)
	// Older seq is logged but not applied. Omitted title/link/progress stay.
	mustApply(t, s, Update{Agent: "bot", JobID: "j", State: "failed", Seq: i64Ptr(2), Title: strPtr("no")})
	cur = cur.Add(2 * time.Second)
	mustApply(t, s, Update{Agent: "bot", JobID: "j", State: "blocked", Note: strPtr("still going")})
	cur = cur.Add(2 * time.Second)
	mustApply(t, s, Update{Agent: "other", JobID: "z", State: "done", Title: strPtr("Ship")})

	before := dumpJobs(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := openClock(t, dir, nowFn)
	after := dumpJobs(t, s2)
	if before != after {
		t.Fatalf("restart mismatch\n got %s\nwant %s", after, before)
	}

	deleted, err := s2.Delete("bot", "j")
	if err != nil || !deleted {
		t.Fatalf("delete: %v %v", deleted, err)
	}
	if _, ok := s2.Get("bot", "j"); ok {
		t.Fatal("job still present")
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}

	s3 := openClock(t, dir, nowFn)
	defer s3.Close()
	if _, ok := s3.Get("bot", "j"); ok {
		t.Fatal("tombstone did not survive restart")
	}
	if _, ok := s3.Get("other", "z"); !ok {
		t.Fatal("other job disappeared")
	}
	cur = cur.Add(time.Second)
	mustApply(t, s3, Update{Agent: "bot", JobID: "j", State: "queued", Title: strPtr("again")})
	job, ok := s3.Get("bot", "j")
	if !ok || job.Title != "again" || len(job.History) != 1 {
		t.Fatalf("recreated job: %+v", job)
	}
}

func TestCompactionKeepsMergedState(t *testing.T) {
	dir := t.TempDir()
	cur := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return cur }
	s := openClock(t, dir, nowFn)

	const n = 1100
	for i := 0; i < n; i++ {
		if i > 0 {
			cur = cur.Add(time.Second)
		}
		u := Update{
			Agent: "bot", JobID: "j", State: "running",
			Seq:   i64Ptr(int64(i + 1)),
			Title: strPtr("t" + itoa(i)),
		}
		p := i % 101
		u.Progress = &p
		if i == 0 {
			u.Link = strPtr("http://example.test/build")
		}
		if i == 10 {
			u.Note = strPtr("hello")
		}
		mustApply(t, s, u)
	}
	before := dumpJobs(t, s)
	job, _ := s.Get("bot", "j")
	if job.Title != "t1099" || job.Link != "http://example.test/build" || job.Note != "hello" || len(job.History) != maxHist {
		t.Fatalf("pre-compact job title=%s link=%s note=%s hist=%d", job.Title, job.Link, job.Note, len(job.History))
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got := countLines(t, dir); got != n {
		t.Fatalf("raw lines = %d", got)
	}

	s2 := openClock(t, dir, nowFn)
	if dumpJobs(t, s2) != before {
		t.Fatal("replay before compact diverged")
	}
	if s2.lines != 2 {
		t.Fatalf("compacted lines in memory = %d", s2.lines)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	if got := countLines(t, dir); got != 2 {
		t.Fatalf("compacted file lines = %d", got)
	}

	s3 := openClock(t, dir, nowFn)
	defer s3.Close()
	if dumpJobs(t, s3) != before {
		t.Fatalf("snapshot replay diverged\n got %s\nwant %s", dumpJobs(t, s3), before)
	}
}

func TestBelowThresholdDoesNotCompact(t *testing.T) {
	dir := t.TempDir()
	s := openClock(t, dir, func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })
	for i := 0; i < 3; i++ {
		mustApply(t, s, Update{Agent: "a", JobID: "j", State: "running", Seq: i64Ptr(int64(i + 1))})
	}
	s.Close()
	s2 := openClock(t, dir, func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })
	s2.Close()
	if got := countLines(t, dir); got != 3 {
		t.Fatalf("lines = %d, compaction ran too early", got)
	}
}

func TestHistoryCapAndSort(t *testing.T) {
	dir := t.TempDir()
	cur := time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)
	s := openClock(t, dir, func() time.Time { return cur })
	defer s.Close()

	for i := 0; i < 25; i++ {
		cur = cur.Add(time.Second)
		mustApply(t, s, Update{Agent: "a", JobID: "old", State: "queued", Seq: i64Ptr(int64(i + 1))})
	}
	job, _ := s.Get("a", "old")
	if len(job.History) != maxHist {
		t.Fatalf("history = %d", len(job.History))
	}
	if job.History[0].N != 6 {
		t.Fatalf("oldest kept n = %d", job.History[0].N)
	}
	_, events := s.Events(1000)
	if len(events) != 25 {
		t.Fatalf("recent = %d", len(events))
	}

	cur = cur.Add(time.Second)
	mustApply(t, s, Update{Agent: "b", JobID: "only", State: "running"})
	cur = cur.Add(time.Second)
	mustApply(t, s, Update{Agent: "a", JobID: "new", State: "running"})
	_, jobs := s.List()
	if len(jobs) != 3 || jobs[0].JobID != "new" || jobs[1].JobID != "old" || jobs[2].Agent != "b" {
		t.Fatalf("sort = %s/%s %s/%s %s/%s", jobs[0].Agent, jobs[0].JobID, jobs[1].Agent, jobs[1].JobID, jobs[2].Agent, jobs[2].JobID)
	}
}

func TestIngestDoesNotBlockOnSlowSubscriber(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, ch, cancel := s.Subscribe()
	defer cancel()
	_ = ch
	start := time.Now()
	for i := 0; i < 100; i++ {
		mustApply(t, s, Update{Agent: "a", JobID: "j", State: "running", Seq: i64Ptr(int64(i + 1)), Title: strPtr(itoa(i))})
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("ingest took %s", time.Since(start))
	}
}

func TestConcurrentApply(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.Apply(Update{Agent: "a", JobID: "j" + itoa(i), State: "running"}); err != nil {
				t.Errorf("apply %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	_, jobs := s.List()
	if len(jobs) != 20 {
		t.Fatalf("jobs = %d", len(jobs))
	}
}

func TestSecondOpenIsLocked(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := Open(dir); err == nil {
		t.Fatal("expected lock error")
	}
}

func TestOpenCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "statuspan")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
}

func TestTrimTail(t *testing.T) {
	got := trimTail([]int{1, 2, 3, 4, 5}, 3)
	if len(got) != 3 || got[0] != 3 || got[2] != 5 {
		t.Fatalf("%v", got)
	}
	if len(trimTail([]int{1}, 10)) != 1 {
		t.Fatal("short slice")
	}
}

func TestBlockedNeverStalls(t *testing.T) {
	dir := t.TempDir()
	cur := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	s := openClock(t, dir, func() time.Time { return cur })
	defer s.Close()

	ttl := 5
	mustApply(t, s, Update{Agent: "bot", JobID: "b", State: "blocked", TTL: &ttl, Title: strPtr("wait")})
	mustApply(t, s, Update{Agent: "bot", JobID: "r", State: "running", TTL: &ttl})
	mustApply(t, s, Update{Agent: "bot", JobID: "q", State: "queued", TTL: &ttl})
	mustApply(t, s, Update{Agent: "bot", JobID: "f", State: "failed", TTL: &ttl})
	mustApply(t, s, Update{Agent: "bot", JobID: "d", State: "done", TTL: &ttl})
	cur = cur.Add(6 * time.Second)

	got := map[string]bool{}
	for _, id := range []string{"b", "r", "q", "f", "d"} {
		job, ok := s.Get("bot", id)
		if !ok {
			t.Fatalf("missing %s", id)
		}
		got[id] = job.Stalled
	}
	if got["b"] || got["f"] || got["d"] || got["q"] || !got["r"] {
		t.Fatalf("stalled = %#v", got)
	}

	// A running job that becomes blocked stays unstalled after the ttl elapses again.
	mustApply(t, s, Update{Agent: "bot", JobID: "r", State: "blocked"})
	cur = cur.Add(6 * time.Second)
	job, _ := s.Get("bot", "r")
	if job.Stalled || job.State != "blocked" {
		t.Fatalf("blocked after ttl: state=%s stalled=%v", job.State, job.Stalled)
	}
}

func TestOmitStateKeepsPrevious(t *testing.T) {
	dir := t.TempDir()
	cur := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return cur }
	s := openClock(t, dir, nowFn)

	if _, err := s.Apply(Update{Agent: "bot", JobID: "j", Note: strPtr("nope")}); err == nil {
		t.Fatal("new job without state should fail")
	}
	if _, ok := s.Get("bot", "j"); ok {
		t.Fatal("rejected job was stored")
	}
	if got := countLines(t, dir); got != 0 {
		t.Fatalf("rejected update wrote %d lines", got)
	}
	if err := s.ValidateBatch([]Update{{Agent: "bot", JobID: "j", Note: strPtr("nope")}}); err == nil {
		t.Fatal("batch should reject a new job without state")
	}

	mustApply(t, s, Update{
		Agent: "bot", JobID: "j", State: "running", Seq: i64Ptr(3),
		Title: strPtr("Build"), Progress: intPtr(10), Link: strPtr("http://example.test/a"), TTL: intPtr(30),
	})
	cur = cur.Add(time.Second)
	if !mustApply(t, s, Update{Agent: "bot", JobID: "j", Note: strPtr("still"), Progress: intPtr(25)}) {
		t.Fatal("partial update should apply")
	}
	job, _ := s.Get("bot", "j")
	if job.State != "running" || job.Title != "Build" || job.Progress != 25 || job.Note != "still" || job.Link != "http://example.test/a" {
		t.Fatalf("partial merge: %+v", job)
	}
	if job.Seq == nil || *job.Seq != 3 {
		t.Fatalf("seq changed: %v", job.Seq)
	}
	last := job.History[len(job.History)-1]
	if last.State != "" || last.Note != "still" || !last.Applied {
		t.Fatalf("partial event: %+v", last)
	}

	lines := readLines(t, dir)
	if len(lines) != 2 || strings.Contains(lines[1], `"state"`) {
		t.Fatalf("log lines = %#v", lines)
	}

	before := countLines(t, dir)
	if _, err := s.Apply(Update{Agent: "bot", JobID: "j", State: "Nope"}); err == nil {
		t.Fatal("invalid state should fail")
	}
	if got := countLines(t, dir); got != before {
		t.Fatalf("invalid state wrote a line (%d to %d)", before, got)
	}
	job, _ = s.Get("bot", "j")
	if job.State != "running" {
		t.Fatalf("state changed on invalid update: %s", job.State)
	}

	cur = cur.Add(time.Second)
	doneAt := cur
	mustApply(t, s, Update{Agent: "bot", JobID: "j", State: "done"})
	cur = cur.Add(time.Second)
	mustApply(t, s, Update{Agent: "bot", JobID: "j", Note: strPtr("shipped")})
	job, _ = s.Get("bot", "j")
	if job.State != "done" || job.Note != "shipped" || job.FinishedAt == nil || !job.FinishedAt.Equal(doneAt) {
		t.Fatalf("done partial: state=%s note=%s finished=%v", job.State, job.Note, job.FinishedAt)
	}

	batch := []Update{
		{Agent: "bot", JobID: "other", State: "queued", Title: strPtr("Q")},
		{Agent: "bot", JobID: "other", Note: strPtr("queued-note")},
	}
	if err := s.ValidateBatch(batch); err != nil {
		t.Fatal(err)
	}
	for _, u := range batch {
		mustApply(t, s, u)
	}
	other, _ := s.Get("bot", "other")
	if other.State != "queued" || other.Note != "queued-note" || other.Title != "Q" {
		t.Fatalf("batch partial: %+v", other)
	}
	bad := []Update{
		{Agent: "bot", JobID: "fresh", State: "running"},
		{Agent: "bot", JobID: "missing"},
	}
	if err := s.ValidateBatch(bad); err == nil {
		t.Fatal("batch with a new job missing state should fail")
	}

	snap := dumpJobs(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := openClock(t, dir, nowFn)
	defer s2.Close()
	if dumpJobs(t, s2) != snap {
		t.Fatalf("replay mismatch\n got %s\nwant %s", dumpJobs(t, s2), snap)
	}
}

func TestAgentEventsKeepLogHistory(t *testing.T) {
	dir := t.TempDir()
	cur := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return cur }
	s := openClock(t, dir, nowFn)

	const n = 30
	for i := 0; i < n; i++ {
		cur = cur.Add(time.Second)
		mustApply(t, s, Update{
			Agent: "bot", JobID: "j", State: "running", Seq: i64Ptr(int64(i + 1)), Note: strPtr(itoa(i)),
		})
	}
	cur = cur.Add(time.Second)
	mustApply(t, s, Update{Agent: "bot", JobID: "j", Note: strPtr("partial")})
	mustApply(t, s, Update{Agent: "other", JobID: "z", State: "queued", Title: strPtr("nope")})

	job, _ := s.Get("bot", "j")
	if len(job.History) != maxHist {
		t.Fatalf("history = %d", len(job.History))
	}
	_, evs := s.AgentEvents("bot")
	if len(evs) != n+1 {
		t.Fatalf("agent events = %d", len(evs))
	}
	if evs[0].Note != "partial" || evs[0].State != "" || evs[len(evs)-1].Note != "0" {
		t.Fatalf("order newest=%+v oldest=%+v", evs[0], evs[len(evs)-1])
	}
	_, other := s.AgentEvents("other")
	if len(other) != 1 || other[0].JobID != "z" {
		t.Fatalf("other = %+v", other)
	}
	_, none := s.AgentEvents("missing")
	if len(none) != 0 {
		t.Fatalf("missing = %d", len(none))
	}
	_, recent := s.Events(1000)
	if len(recent) != n+2 {
		t.Fatalf("recent = %d", len(recent))
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openClock(t, dir, nowFn)
	_, evs = s.AgentEvents("bot")
	if len(evs) != n+1 || evs[0].Note != "partial" {
		t.Fatalf("replayed agent events = %d note=%s", len(evs), evs[0].Note)
	}
	if _, err := s.Delete("bot", "j"); err != nil {
		t.Fatal(err)
	}
	_, evs = s.AgentEvents("bot")
	if len(evs) != n+2 || evs[0].Kind != "delete" {
		t.Fatalf("after delete: %d kind=%s", len(evs), evs[0].Kind)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openClock(t, dir, nowFn)
	defer s.Close()
	_, evs = s.AgentEvents("bot")
	if len(evs) != n+2 || evs[0].Kind != "delete" || evs[1].Note != "partial" {
		t.Fatalf("delete replay: %d %+v", len(evs), evs[0])
	}
	if _, ok := s.Get("bot", "j"); ok {
		t.Fatal("deleted job came back")
	}
}

func TestAgentEventsNotTrimmedToRecent(t *testing.T) {
	dir := t.TempDir()
	cur := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	s := openClock(t, dir, func() time.Time { return cur })

	const n = maxRecent + 5
	for i := 0; i < n; i++ {
		if i > 0 {
			cur = cur.Add(time.Second)
		}
		mustApply(t, s, Update{Agent: "bot", JobID: "j", State: "running", Seq: i64Ptr(int64(i + 1))})
	}
	job, _ := s.Get("bot", "j")
	if len(job.History) != maxHist {
		t.Fatalf("history = %d", len(job.History))
	}
	_, recent := s.Events(n + 10)
	if len(recent) != maxRecent {
		t.Fatalf("recent = %d", len(recent))
	}
	_, all := s.AgentEvents("bot")
	if len(all) != n || all[0].N != int64(n) || all[len(all)-1].N != 1 {
		t.Fatalf("agent events = %d first=%d last=%d", len(all), all[0].N, all[len(all)-1].N)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := openClock(t, dir, func() time.Time { return cur })
	defer s2.Close()
	_, all = s2.AgentEvents("bot")
	if len(all) != n {
		t.Fatalf("replayed agent events = %d, log was trimmed early", len(all))
	}
	_, recent = s2.Events(n + 10)
	if len(recent) != maxRecent {
		t.Fatalf("replayed recent = %d", len(recent))
	}
}

func readLines(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, logName))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func openAbandon(t *testing.T, dir string, start time.Time, after, waiting *time.Duration) (*Store, *time.Time) {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	cur := start
	now := &cur
	s, err := OpenWith(dir, Options{
		Now:                 func() time.Time { return *now },
		AbandonAfter:        after,
		AbandonWaitingAfter: waiting,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, now
}

func TestDefaultAbandonWindows(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	after, waiting := s.AbandonPolicySeconds()
	if after != int64((24*time.Hour)/time.Second) || waiting != int64((168*time.Hour)/time.Second) {
		t.Fatalf("policy seconds after=%d waiting=%d", after, waiting)
	}
	snap, _, cancel := s.Subscribe()
	defer cancel()
	if snap.AbandonAfterSeconds != after || snap.AbandonWaitingAfterSeconds != waiting {
		t.Fatalf("snapshot policy %+v", snap)
	}
}

func TestAbandoned(t *testing.T) {
	start := time.Date(2026, 11, 22, 0, 0, 11, 0, time.UTC)

	t.Run("running just after ttl is stalled not abandoned", func(t *testing.T) {
		s, now := openAbandon(t, "", start, nil, nil)
		ttl := 7200
		mustApply(t, s, Update{Agent: "bot", JobID: "run", State: "running", TTL: &ttl, Seq: i64Ptr(1)})
		*now = start.Add(time.Duration(ttl)*time.Second + time.Millisecond)
		job, ok := s.Get("bot", "run")
		if !ok || !job.Stalled || job.Abandoned || job.AbandonedAt != nil || job.State != "running" {
			t.Fatalf("%+v", job)
		}
	})

	t.Run("24h wins when 4*ttl is shorter", func(t *testing.T) {
		dir := t.TempDir()
		s, now := openAbandon(t, dir, start, nil, nil)
		ttl := 7200 // 4*ttl = 8h, so the threshold is the 24h default
		mustApply(t, s, Update{Agent: "bot", JobID: "run", State: "running", TTL: &ttl, Title: strPtr("launch"), Seq: i64Ptr(1)})
		*now = start.Add(8*time.Hour + time.Second)
		job, _ := s.Get("bot", "run")
		if !job.Stalled || job.Abandoned {
			t.Fatalf("inside 24h: stalled=%v abandoned=%v", job.Stalled, job.Abandoned)
		}
		*now = start.Add(24 * time.Hour)
		job, _ = s.Get("bot", "run")
		if job.Abandoned || !job.Stalled || job.AbandonedAt != nil {
			t.Fatalf("at 24h: stalled=%v abandoned=%v at=%v", job.Stalled, job.Abandoned, job.AbandonedAt)
		}
		*now = start.Add(24*time.Hour + time.Nanosecond)
		job, _ = s.Get("bot", "run")
		wantAt := start.Add(24 * time.Hour)
		if !job.Abandoned || job.Stalled || job.State != "running" || job.AbandonedAt == nil || !job.AbandonedAt.Equal(wantAt) {
			t.Fatalf("just after 24h: %+v", job)
		}

		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		replayed, err := OpenWith(dir, Options{Now: func() time.Time { return *now }})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = replayed.Close() })
		job, _ = replayed.Get("bot", "run")
		if !job.Abandoned || job.Stalled || job.State != "running" || job.AbandonedAt == nil || !job.AbandonedAt.Equal(wantAt) {
			t.Fatalf("replayed: %+v", job)
		}
		for _, line := range readLines(t, dir) {
			if strings.Contains(line, "abandoned") {
				t.Fatalf("log contains abandoned: %s", line)
			}
		}
	})

	t.Run("large ttl uses 4*ttl", func(t *testing.T) {
		s, now := openAbandon(t, "", start, nil, nil)
		ttl := 7 * 3600 // 4*ttl = 28h > 24h
		mustApply(t, s, Update{Agent: "bot", JobID: "run", State: "running", TTL: &ttl})
		*now = start.Add(24*time.Hour + time.Second)
		job, _ := s.Get("bot", "run")
		if !job.Stalled || job.Abandoned {
			t.Fatalf("24h with large ttl: stalled=%v abandoned=%v", job.Stalled, job.Abandoned)
		}
		threshold := 28 * time.Hour
		*now = start.Add(threshold)
		job, _ = s.Get("bot", "run")
		if job.Abandoned || !job.Stalled {
			t.Fatalf("at 4*ttl: stalled=%v abandoned=%v", job.Stalled, job.Abandoned)
		}
		*now = start.Add(threshold + time.Millisecond)
		job, _ = s.Get("bot", "run")
		if !job.Abandoned || job.Stalled || job.AbandonedAt == nil || !job.AbandonedAt.Equal(start.Add(threshold)) {
			t.Fatalf("just after 4*ttl: %+v", job)
		}
	})

	t.Run("ttl zero abandons at abandon-after", func(t *testing.T) {
		s, now := openAbandon(t, "", start, nil, nil)
		ttl := 0
		mustApply(t, s, Update{Agent: "bot", JobID: "run", State: "running", TTL: &ttl})
		*now = start.Add(time.Millisecond)
		job, _ := s.Get("bot", "run")
		if !job.Stalled || job.Abandoned {
			t.Fatalf("ttl 0 fresh stall: %+v", job)
		}
		*now = start.Add(24*time.Hour + time.Nanosecond)
		job, _ = s.Get("bot", "run")
		if !job.Abandoned || job.Stalled {
			t.Fatalf("ttl 0 after 24h: stalled=%v abandoned=%v", job.Stalled, job.Abandoned)
		}
	})

	t.Run("applied post revives", func(t *testing.T) {
		s, now := openAbandon(t, "", start, nil, nil)
		ttl := 60
		mustApply(t, s, Update{Agent: "bot", JobID: "run", State: "running", TTL: &ttl, Seq: i64Ptr(1), Title: strPtr("work")})
		*now = start.Add(25 * time.Hour)
		job, _ := s.Get("bot", "run")
		if !job.Abandoned || job.Stalled {
			t.Fatal("expected abandoned before revival")
		}
		if !mustApply(t, s, Update{Agent: "bot", JobID: "run", State: "running", Seq: i64Ptr(2), Note: strPtr("back")}) {
			t.Fatal("newer seq should apply")
		}
		job, _ = s.Get("bot", "run")
		if job.Abandoned || job.Stalled || job.AbandonedAt != nil || job.Note != "back" || job.State != "running" || !job.UpdatedAt.Equal(*now) {
			t.Fatalf("revived: %+v", job)
		}
	})

	t.Run("older seq does not revive", func(t *testing.T) {
		s, now := openAbandon(t, "", start, nil, nil)
		ttl := 60
		mustApply(t, s, Update{Agent: "bot", JobID: "run", State: "running", TTL: &ttl, Seq: i64Ptr(5), Title: strPtr("work")})
		*now = start.Add(25 * time.Hour)
		before, _ := s.Get("bot", "run")
		if !before.Abandoned {
			t.Fatal("expected abandoned")
		}
		if mustApply(t, s, Update{Agent: "bot", JobID: "run", State: "failed", Seq: i64Ptr(4), Title: strPtr("nope")}) {
			t.Fatal("older seq applied")
		}
		job, _ := s.Get("bot", "run")
		if !job.Abandoned || job.Stalled || job.State != "running" || job.Title != "work" || !job.UpdatedAt.Equal(before.UpdatedAt) {
			t.Fatalf("unapplied seq changed the job: %+v", job)
		}
		if job.AbandonedAt == nil || !job.AbandonedAt.Equal(before.UpdatedAt.Add(24*time.Hour)) {
			t.Fatalf("abandoned_at = %v", job.AbandonedAt)
		}
		if len(job.History) == 0 || job.History[len(job.History)-1].Applied {
			t.Fatalf("history = %+v", job.History)
		}
	})

	t.Run("queued and blocked wait 7d", func(t *testing.T) {
		s, now := openAbandon(t, "", start, nil, nil)
		ttl := 10
		for _, state := range []string{"queued", "blocked"} {
			mustApply(t, s, Update{Agent: "bot", JobID: state, State: state, TTL: &ttl})
		}
		*now = start.Add(24*time.Hour + time.Second)
		for _, state := range []string{"queued", "blocked"} {
			job, _ := s.Get("bot", state)
			if job.Abandoned || job.Stalled || job.State != state {
				t.Fatalf("%s at 24h: %+v", state, job)
			}
		}
		*now = start.Add(7 * 24 * time.Hour)
		for _, state := range []string{"queued", "blocked"} {
			job, _ := s.Get("bot", state)
			if job.Abandoned || job.Stalled {
				t.Fatalf("%s at 7d: %+v", state, job)
			}
		}
		*now = start.Add(7*24*time.Hour + time.Nanosecond)
		wantAt := start.Add(7 * 24 * time.Hour)
		for _, state := range []string{"queued", "blocked"} {
			job, _ := s.Get("bot", state)
			if !job.Abandoned || job.Stalled || job.State != state || job.AbandonedAt == nil || !job.AbandonedAt.Equal(wantAt) {
				t.Fatalf("%s after 7d: %+v", state, job)
			}
		}
	})

	t.Run("done and failed never abandon", func(t *testing.T) {
		s, now := openAbandon(t, "", start, nil, nil)
		mustApply(t, s, Update{Agent: "bot", JobID: "d", State: "done"})
		mustApply(t, s, Update{Agent: "bot", JobID: "f", State: "failed"})
		*now = start.Add(30 * 24 * time.Hour)
		for _, id := range []string{"d", "f"} {
			job, _ := s.Get("bot", id)
			if job.Abandoned || job.Stalled || job.AbandonedAt != nil {
				t.Fatalf("%s: %+v", id, job)
			}
		}
	})

	t.Run("zero disables both", func(t *testing.T) {
		zero := time.Duration(0)
		s, now := openAbandon(t, "", start, &zero, &zero)
		after, waiting := s.AbandonPolicySeconds()
		if after != 0 || waiting != 0 {
			t.Fatalf("policy after=%d waiting=%d", after, waiting)
		}
		ttl := 10
		mustApply(t, s, Update{Agent: "bot", JobID: "run", State: "running", TTL: &ttl})
		mustApply(t, s, Update{Agent: "bot", JobID: "q", State: "queued", TTL: &ttl})
		mustApply(t, s, Update{Agent: "bot", JobID: "b", State: "blocked", TTL: &ttl})
		*now = start.Add(30 * 24 * time.Hour)
		run, _ := s.Get("bot", "run")
		if !run.Stalled || run.Abandoned {
			t.Fatalf("running: stalled=%v abandoned=%v", run.Stalled, run.Abandoned)
		}
		for _, id := range []string{"q", "b"} {
			job, _ := s.Get("bot", id)
			if job.Abandoned || job.Stalled {
				t.Fatalf("%s: %+v", id, job)
			}
		}
	})

	t.Run("zero running still abandons queued", func(t *testing.T) {
		zero := time.Duration(0)
		s, now := openAbandon(t, "", start, &zero, nil)
		ttl := 10
		mustApply(t, s, Update{Agent: "bot", JobID: "run", State: "running", TTL: &ttl})
		mustApply(t, s, Update{Agent: "bot", JobID: "q", State: "queued"})
		*now = start.Add(8 * 24 * time.Hour)
		run, _ := s.Get("bot", "run")
		queued, _ := s.Get("bot", "q")
		if run.Abandoned || !run.Stalled || !queued.Abandoned || queued.Stalled {
			t.Fatalf("run=%+v queued=%+v", run, queued)
		}
	})

	t.Run("zero waiting still abandons running", func(t *testing.T) {
		zero := time.Duration(0)
		s, now := openAbandon(t, "", start, nil, &zero)
		ttl := 10
		mustApply(t, s, Update{Agent: "bot", JobID: "run", State: "running", TTL: &ttl})
		mustApply(t, s, Update{Agent: "bot", JobID: "q", State: "queued"})
		*now = start.Add(25 * time.Hour)
		run, _ := s.Get("bot", "run")
		queued, _ := s.Get("bot", "q")
		if !run.Abandoned || run.Stalled || queued.Abandoned || queued.Stalled {
			t.Fatalf("run=%+v queued=%+v", run, queued)
		}
	})

	t.Run("log is not rewritten", func(t *testing.T) {
		dir := t.TempDir()
		s, now := openAbandon(t, dir, start, nil, nil)
		ttl := 30
		mustApply(t, s, Update{Agent: "bot", JobID: "run", State: "running", TTL: &ttl, Seq: i64Ptr(1)})
		*now = start.Add(48 * time.Hour)
		before, err := os.ReadFile(filepath.Join(dir, logName))
		if err != nil {
			t.Fatal(err)
		}
		job, _ := s.Get("bot", "run")
		if !job.Abandoned || job.State != "running" {
			t.Fatalf("%+v", job)
		}
		_, _ = s.List()
		after, err := os.ReadFile(filepath.Join(dir, logName))
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) {
			t.Fatal("reading an abandoned job rewrote the log")
		}
		if strings.Contains(string(after), "abandoned") {
			t.Fatalf("log contains abandoned: %s", after)
		}
		if _, err := s.Apply(Update{Agent: "bot", JobID: "run", State: "abandoned", Seq: i64Ptr(2)}); err == nil {
			t.Fatal("state=abandoned was accepted")
		}
		afterReject, err := os.ReadFile(filepath.Join(dir, logName))
		if err != nil {
			t.Fatal(err)
		}
		if string(afterReject) != string(before) {
			t.Fatal("rejected abandoned post changed the log")
		}
	})

	t.Run("snapshot omits abandoned", func(t *testing.T) {
		job := &jobState{
			agent: "bot", jobID: "run", state: "running", ttl: 30,
			startedAt: start, updatedAt: start,
		}
		raw, err := json.Marshal(job.snap())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "abandon") {
			t.Fatalf("snap: %s", raw)
		}
		if !strings.Contains(string(raw), `"state":"running"`) {
			t.Fatalf("snap: %s", raw)
		}
	})

	t.Run("ttl overflow is not abandoned", func(t *testing.T) {
		s, now := openAbandon(t, "", start, nil, nil)
		ttl := int(^uint(0) >> 1)
		mustApply(t, s, Update{Agent: "bot", JobID: "run", State: "running", TTL: &ttl})
		*now = start.Add(48 * time.Hour)
		job, _ := s.Get("bot", "run")
		if job.Stalled || job.Abandoned {
			t.Fatalf("overflow: stalled=%v abandoned=%v", job.Stalled, job.Abandoned)
		}
	})
}

func TestClearBacksUpAndReplaysEmpty(t *testing.T) {
	dir := t.TempDir()
	cur := time.Date(2026, 11, 22, 20, 30, 0, 0, time.UTC)
	s := openClock(t, dir, func() time.Time { return cur })
	mustApply(t, s, Update{
		Agent: "bot", JobID: "j", State: "running", Title: strPtr("Keep me"),
		Note: strPtr("history note"), Seq: i64Ptr(1),
	})
	cur = cur.Add(time.Second)
	mustApply(t, s, Update{Agent: "bot", JobID: "j", Progress: intPtr(40), Note: strPtr("still here")})
	mustApply(t, s, Update{Agent: "other", JobID: "z", State: "done", Title: strPtr("Ship")})

	_, ch, cancel := s.Subscribe()
	defer cancel()

	n, backup, err := s.Clear()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("deleted %d", n)
	}
	if !filepath.IsAbs(backup) {
		t.Fatalf("backup path %q", backup)
	}
	raw, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "Keep me") || !strings.Contains(string(raw), "history note") || !strings.Contains(string(raw), "Ship") {
		t.Fatalf("backup missing prior events: %s", raw)
	}
	select {
	case push := <-ch:
		if push.Snapshot == nil || len(push.Snapshot.Jobs) != 0 || len(push.Snapshot.Events) != 0 {
			t.Fatalf("snapshot %+v", push.Snapshot)
		}
	case <-time.After(time.Second):
		t.Fatal("clear did not publish a snapshot")
	}
	if _, jobs := s.List(); len(jobs) != 0 {
		t.Fatalf("jobs left %d", len(jobs))
	}
	if _, ev := s.Events(50); len(ev) != 0 {
		t.Fatalf("events left %d", len(ev))
	}
	if _, ev := s.AgentEvents("bot"); len(ev) != 0 {
		t.Fatalf("agent history left %d", len(ev))
	}
	live, err := os.ReadFile(filepath.Join(dir, logName))
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.TrimSpace(string(live))) != 0 {
		t.Fatalf("log not empty: %q", live)
	}
	if _, err := s.Apply(Update{Agent: "bot", JobID: "mid", State: "queued", Title: strPtr("Mid")}); err != nil {
		t.Fatal(err)
	}
	if job, ok := s.Get("bot", "mid"); !ok || job.Title != "Mid" {
		t.Fatalf("post after clear: %+v ok=%v", job, ok)
	}
	midLog, err := os.ReadFile(filepath.Join(dir, logName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(midLog), "Mid") {
		t.Fatalf("truncated log did not accept a post: %q", midLog)
	}

	n2, backup2, err := s.Clear()
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 1 {
		t.Fatalf("second deleted %d", n2)
	}
	if backup2 == backup {
		t.Fatalf("second clear reused %s", backup2)
	}
	if _, err := os.Stat(backup2); err != nil {
		t.Fatal(err)
	}
	still, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if string(still) != string(raw) {
		t.Fatal("second clear rewrote the first backup")
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, jobs := reopened.List(); len(jobs) != 0 {
		t.Fatalf("replay jobs %d", len(jobs))
	}
	if _, ev := reopened.Events(50); len(ev) != 0 {
		t.Fatalf("replay events %d", len(ev))
	}
	if _, ev := reopened.AgentEvents("bot"); len(ev) != 0 {
		t.Fatalf("replay agent events %d", len(ev))
	}
	if _, err := reopened.Apply(Update{Agent: "bot", JobID: "fresh", State: "queued", Title: strPtr("After")}); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	_, jobs := again.List()
	if len(jobs) != 1 || jobs[0].JobID != "fresh" || jobs[0].Title != "After" {
		t.Fatalf("after clear replay %+v", jobs)
	}
	if _, ok := again.Get("bot", "j"); ok {
		t.Fatal("cleared job returned")
	}
}

func TestBoardSection(t *testing.T) {
	cases := []struct {
		name string
		v    JobView
		want string
	}{
		{"failed", JobView{State: "failed"}, "attention"},
		{"blocked", JobView{State: "blocked"}, "attention"},
		{"stalled running", JobView{State: "running", Stalled: true}, "attention"},
		{"abandoned running", JobView{State: "running", Abandoned: true}, "attention"},
		{"abandoned queued", JobView{State: "queued", Abandoned: true}, "attention"},
		{"abandoned blocked", JobView{State: "blocked", Abandoned: true}, "attention"},
		{"healthy running", JobView{State: "running"}, "progress"},
		{"queued", JobView{State: "queued"}, "queued"},
		{"done", JobView{State: "done"}, "done"},
		{"done ignores abandoned flag", JobView{State: "done", Abandoned: true}, "done"},
		{"failed ignores stalled flag", JobView{State: "failed", Stalled: true}, "attention"},
		{"empty", JobView{}, ""},
	}
	for _, tc := range cases {
		if got := BoardSection(tc.v); got != tc.want {
			t.Fatalf("%s: %q want %q", tc.name, got, tc.want)
		}
	}
}

func seedClearBoard(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	start := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	after := 20 * time.Minute
	waiting := 30 * time.Minute
	s, now := openAbandon(t, dir, start, &after, &waiting)
	early := []Update{
		{Agent: "bot", JobID: "abandon-run", State: "running", TTL: intPtr(10), Title: strPtr("Abandon run")},
		{Agent: "bot", JobID: "abandon-q", State: "queued", Title: strPtr("Abandon queue")},
		{Agent: "bot", JobID: "block-old", State: "blocked", Title: strPtr("Old block")},
		{Agent: "bot", JobID: "fail", State: "failed", Title: strPtr("Fail")},
		{Agent: "other", JobID: "done", State: "done", Title: strPtr("Done job"), Note: strPtr("shipped")},
	}
	for _, u := range early {
		mustApply(t, s, u)
	}
	*now = start.Add(34 * time.Minute)
	late := []Update{
		{Agent: "bot", JobID: "stall", State: "running", TTL: intPtr(10), Title: strPtr("Stall")},
		{Agent: "bot", JobID: "run", State: "running", TTL: intPtr(600), Title: strPtr("Running"), Note: strPtr("keep")},
		{Agent: "bot", JobID: "queue", State: "queued", Title: strPtr("Queue")},
		{Agent: "bot", JobID: "block", State: "blocked", Title: strPtr("Block")},
	}
	for _, u := range late {
		mustApply(t, s, u)
	}
	*now = start.Add(35 * time.Minute)
	want := map[string]string{
		"abandon-run": "attention",
		"abandon-q":   "attention",
		"block-old":   "attention",
		"fail":        "attention",
		"stall":       "attention",
		"block":       "attention",
		"run":         "progress",
		"queue":       "queued",
		"done":        "done",
	}
	_, jobs := s.List()
	if len(jobs) != len(want) {
		t.Fatalf("seed jobs %d", len(jobs))
	}
	byID := map[string]JobView{}
	for _, j := range jobs {
		byID[j.JobID] = j
		if got := BoardSection(j); got != want[j.JobID] {
			t.Fatalf("%s section %q want %q stalled=%v abandoned=%v state=%s", j.JobID, got, want[j.JobID], j.Stalled, j.Abandoned, j.State)
		}
	}
	if !byID["stall"].Stalled || byID["stall"].Abandoned {
		t.Fatalf("stall %+v", byID["stall"])
	}
	if !byID["abandon-run"].Abandoned || byID["abandon-run"].Stalled || byID["abandon-run"].State != "running" {
		t.Fatalf("abandon-run %+v", byID["abandon-run"])
	}
	if !byID["abandon-q"].Abandoned || byID["abandon-q"].State != "queued" {
		t.Fatalf("abandon-q %+v", byID["abandon-q"])
	}
	if !byID["block-old"].Abandoned || byID["block-old"].State != "blocked" {
		t.Fatalf("block-old %+v", byID["block-old"])
	}
	if byID["block"].Abandoned || byID["block"].State != "blocked" {
		t.Fatalf("block %+v", byID["block"])
	}
	if byID["run"].Stalled || byID["run"].Abandoned || byID["run"].State != "running" {
		t.Fatalf("run %+v", byID["run"])
	}
	if byID["queue"].Abandoned || byID["queue"].State != "queued" {
		t.Fatalf("queue %+v", byID["queue"])
	}
	return s, dir
}

func assertJobIDs(t *testing.T, s *Store, want ...string) {
	t.Helper()
	_, jobs := s.List()
	got := map[string]bool{}
	for _, j := range jobs {
		got[j.Agent+"/"+j.JobID] = true
	}
	if len(got) != len(want) {
		t.Fatalf("jobs %v want %v", got, want)
	}
	for _, id := range want {
		if !got[id] {
			t.Fatalf("missing %s in %v", id, got)
		}
	}
}

func assertNoBackup(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "jobs-backup-*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("backups %v", matches)
	}
}

func TestClearSection(t *testing.T) {
	allBut := func(drop string) []string {
		ids := []string{
			"bot/abandon-run", "bot/abandon-q", "bot/block-old", "bot/fail",
			"bot/stall", "bot/block", "bot/run", "bot/queue", "other/done",
		}
		out := make([]string, 0, len(ids)-1)
		for _, id := range ids {
			if id != drop {
				out = append(out, id)
			}
		}
		return out
	}
	attentionKept := []string{"bot/run", "bot/queue", "other/done"}

	t.Run("done", func(t *testing.T) {
		s, dir := seedClearBoard(t)
		before := countLines(t, dir)
		n, err := s.ClearSection("done")
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("deleted %d", n)
		}
		assertJobIDs(t, s, allBut("other/done")...)
		job, ok := s.Get("bot", "run")
		if !ok || job.Note != "keep" || job.Title != "Running" {
			t.Fatalf("running %+v ok=%v", job, ok)
		}
		if _, ok := s.Get("other", "done"); ok {
			t.Fatal("done job remained")
		}
		if countLines(t, dir) != before+1 {
			t.Fatalf("log lines %d want %d", countLines(t, dir), before+1)
		}
		assertNoBackup(t, dir)
		raw, err := os.ReadFile(filepath.Join(dir, logName))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "Running") || !strings.Contains(string(raw), `"kind":"delete"`) {
			t.Fatalf("log %s", raw)
		}
	})

	t.Run("attention", func(t *testing.T) {
		s, dir := seedClearBoard(t)
		n, err := s.ClearSection("attention")
		if err != nil {
			t.Fatal(err)
		}
		if n != 6 {
			t.Fatalf("deleted %d", n)
		}
		assertJobIDs(t, s, attentionKept...)
		assertNoBackup(t, dir)
	})

	t.Run("progress", func(t *testing.T) {
		s, dir := seedClearBoard(t)
		n, err := s.ClearSection("progress")
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("deleted %d", n)
		}
		assertJobIDs(t, s, allBut("bot/run")...)
		stall, _ := s.Get("bot", "stall")
		abandoned, _ := s.Get("bot", "abandon-run")
		if !stall.Stalled || stall.Abandoned || !abandoned.Abandoned || abandoned.State != "running" {
			t.Fatalf("stall=%+v abandoned=%+v", stall, abandoned)
		}
		assertNoBackup(t, dir)
	})

	t.Run("queued", func(t *testing.T) {
		s, dir := seedClearBoard(t)
		n, err := s.ClearSection("queued")
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("deleted %d", n)
		}
		assertJobIDs(t, s, allBut("bot/queue")...)
		if job, ok := s.Get("bot", "abandon-q"); !ok || !job.Abandoned || job.State != "queued" {
			t.Fatalf("abandoned queued %+v ok=%v", job, ok)
		}
		assertNoBackup(t, dir)
	})

	t.Run("invalid", func(t *testing.T) {
		s, dir := seedClearBoard(t)
		_, ch, cancel := s.Subscribe()
		defer cancel()
		before, err := os.ReadFile(filepath.Join(dir, logName))
		if err != nil {
			t.Fatal(err)
		}
		snap := dumpJobs(t, s)
		n, err := s.ClearSection("all")
		if n != 0 {
			t.Fatalf("deleted %d", n)
		}
		if _, ok := err.(*ValidationError); !ok {
			t.Fatalf("got %T %v", err, err)
		}
		if dumpJobs(t, s) != snap {
			t.Fatal("invalid section changed the board")
		}
		after, err := os.ReadFile(filepath.Join(dir, logName))
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before) {
			t.Fatal("invalid section changed the log")
		}
		assertNoBackup(t, dir)
		select {
		case push := <-ch:
			t.Fatalf("invalid section published %+v", push.Snapshot)
		default:
		}
	})

	t.Run("empty publishes nothing", func(t *testing.T) {
		dir := t.TempDir()
		cur := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
		s := openClock(t, dir, func() time.Time { return cur })
		defer s.Close()
		mustApply(t, s, Update{Agent: "bot", JobID: "run", State: "running", Title: strPtr("Running"), Note: strPtr("keep")})
		_, ch, cancel := s.Subscribe()
		defer cancel()
		before, err := os.ReadFile(filepath.Join(dir, logName))
		if err != nil {
			t.Fatal(err)
		}
		n, err := s.ClearSection("done")
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("deleted %d", n)
		}
		job, ok := s.Get("bot", "run")
		if !ok || job.Note != "keep" {
			t.Fatalf("%+v ok=%v", job, ok)
		}
		after, err := os.ReadFile(filepath.Join(dir, logName))
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before) {
			t.Fatal("empty section changed the log")
		}
		select {
		case push := <-ch:
			t.Fatalf("empty section published %+v", push.Snapshot)
		case <-time.After(50 * time.Millisecond):
		}
	})

	t.Run("replay and revive", func(t *testing.T) {
		dir := t.TempDir()
		cur := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
		s := openClock(t, dir, func() time.Time { return cur })
		mustApply(t, s, Update{Agent: "bot", JobID: "run", State: "running", Title: strPtr("Stay"), Note: strPtr("history note"), Seq: i64Ptr(1)})
		cur = cur.Add(time.Second)
		mustApply(t, s, Update{Agent: "bot", JobID: "run", Progress: intPtr(40), Note: strPtr("still here")})
		mustApply(t, s, Update{Agent: "bot", JobID: "ship", State: "done", Title: strPtr("Ship"), Note: strPtr("finished")})
		n, err := s.ClearSection("done")
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("deleted %d", n)
		}
		job, ok := s.Get("bot", "run")
		if !ok || job.Title != "Stay" || job.Note != "still here" || job.Progress != 40 || len(job.History) != 2 {
			t.Fatalf("kept %+v ok=%v", job, ok)
		}
		if _, ok := s.Get("bot", "ship"); ok {
			t.Fatal("done job remained")
		}
		assertNoBackup(t, dir)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s = openClock(t, dir, func() time.Time { return cur })
		job, ok = s.Get("bot", "run")
		if !ok || job.Title != "Stay" || job.Note != "still here" || job.Progress != 40 || len(job.History) != 2 {
			t.Fatalf("replay kept %+v ok=%v", job, ok)
		}
		if _, ok := s.Get("bot", "ship"); ok {
			t.Fatal("replay resurrected the cleared job")
		}
		_, evs := s.AgentEvents("bot")
		if len(evs) < 2 || evs[0].Kind != "delete" || evs[0].JobID != "ship" {
			t.Fatalf("agent events %+v", evs)
		}
		cur = cur.Add(time.Second)
		if !mustApply(t, s, Update{Agent: "bot", JobID: "ship", State: "running", Title: strPtr("Back"), Seq: i64Ptr(1)}) {
			t.Fatal("revive did not apply")
		}
		revived, ok := s.Get("bot", "ship")
		if !ok || revived.Title != "Back" || revived.State != "running" {
			t.Fatalf("revived %+v ok=%v", revived, ok)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		again := openClock(t, dir, func() time.Time { return cur })
		defer again.Close()
		revived, ok = again.Get("bot", "ship")
		if !ok || revived.Title != "Back" || revived.State != "running" {
			t.Fatalf("revived replay %+v ok=%v", revived, ok)
		}
		kept, ok := again.Get("bot", "run")
		if !ok || kept.Note != "still here" || kept.Title != "Stay" {
			t.Fatalf("kept replay %+v ok=%v", kept, ok)
		}
	})

	t.Run("snapshot", func(t *testing.T) {
		s, _ := seedClearBoard(t)
		_, ch, cancel := s.Subscribe()
		defer cancel()
		n, err := s.ClearSection("done")
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("deleted %d", n)
		}
		select {
		case push := <-ch:
			if push.Snapshot == nil {
				t.Fatalf("want snapshot, got job %+v", push.Job)
			}
			if push.Job.JobID != "" || push.Job.Deleted {
				t.Fatalf("snapshot push also carried a job %+v", push.Job)
			}
			ids := map[string]bool{}
			for _, j := range push.Snapshot.Jobs {
				ids[j.Agent+"/"+j.JobID] = true
			}
			if ids["other/done"] || !ids["bot/run"] || len(push.Snapshot.Jobs) != 8 {
				t.Fatalf("snapshot jobs %v", ids)
			}
		case <-time.After(time.Second):
			t.Fatal("section clear did not publish a snapshot")
		}
		select {
		case push := <-ch:
			t.Fatalf("extra push snapshot=%v job=%+v", push.Snapshot != nil, push.Job)
		default:
		}
	})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
