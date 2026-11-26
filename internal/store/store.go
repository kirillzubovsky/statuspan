// Package store is the in-memory status board backed by an append-only JSONL log.
// Current state is keyed by (agent, job_id). Every update is appended and fsynced
// before it changes memory, then replayed on the next Open.
package store

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultTTL = 600
	maxHist    = 20
	maxRecent  = 200
	// compactSlack is the extra log lines allowed beyond retained history
	// before startup rewrites the file. Threshold is jobs*maxHist + compactSlack.
	compactSlack = 1000
	// A running job is abandoned only after it is already stalled, once it has
	// been silent for max(abandonAfter, abandonTTLFactor*ttl).
	abandonTTLFactor = 4
	// Defaults apply when Options leaves the window nil. Zero disables that
	// category. Options{} must not disable abandon.
	defaultAbandonAfter        = 24 * time.Hour
	defaultAbandonWaitingAfter = 168 * time.Hour
)

// ValidationError is a rejected update. The HTTP layer maps it to 400.
type ValidationError struct {
	Msg string
}

func (e *ValidationError) Error() string { return e.Msg }

func invalid(msg string) error { return &ValidationError{Msg: msg} }

// Update is one ingested job report. Nil pointers mean the field was omitted
// and must not overwrite the stored value.
type Update struct {
	Agent      string  `json:"agent"`
	JobID      string  `json:"job_id"`
	Title      *string `json:"title,omitempty"`
	State      string  `json:"state"`
	Progress   *int    `json:"progress,omitempty"`
	Link       *string `json:"link,omitempty"`
	Seq        *int64  `json:"seq,omitempty"`
	TTL        *int    `json:"ttl,omitempty"`
	Note       *string `json:"note,omitempty"`
	Via        string  `json:"via,omitempty"`
	IP         string  `json:"ip,omitempty"`
	Country    string  `json:"country,omitempty"`
	ReceivedAt string  `json:"received_at,omitempty"`
}

// Event is one history row. Applied is false when a seq was older than the
// stored seq and the current job fields were left unchanged.
type Event struct {
	N          int64     `json:"n,omitempty"`
	Agent      string    `json:"agent"`
	JobID      string    `json:"job_id"`
	State      string    `json:"state,omitempty"`
	Progress   *int      `json:"progress,omitempty"`
	Note       string    `json:"note,omitempty"`
	Title      string    `json:"title,omitempty"`
	Link       string    `json:"link,omitempty"`
	TS         time.Time `json:"ts"`
	Applied    bool      `json:"applied"`
	Seq        *int64    `json:"seq,omitempty"`
	Kind       string    `json:"kind,omitempty"`
	Via        string    `json:"via,omitempty"`
	IP         string    `json:"ip,omitempty"`
	Country    string    `json:"country,omitempty"`
	ReceivedAt string    `json:"received_at,omitempty"`
}

// JobView is the current job plus computed fields for the board and API.
type JobView struct {
	Agent          string     `json:"agent"`
	JobID          string     `json:"job_id"`
	Title          string     `json:"title"`
	State          string     `json:"state"`
	Progress       int        `json:"progress"`
	Link           string     `json:"link,omitempty"`
	Seq            *int64     `json:"seq,omitempty"`
	TTL            int        `json:"ttl"`
	Note           string     `json:"note,omitempty"`
	Via            string     `json:"via,omitempty"`
	IP             string     `json:"ip,omitempty"`
	Country        string     `json:"country,omitempty"`
	ReceivedAt     string     `json:"received_at,omitempty"`
	StartedAt      time.Time  `json:"started_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	ElapsedSeconds int64      `json:"elapsed_seconds"`
	Stalled        bool       `json:"stalled"`
	// Abandoned is derived at read time, like Stalled. It is not a stored state.
	Abandoned   bool       `json:"abandoned"`
	AbandonedAt *time.Time `json:"abandoned_at,omitempty"`
	History     []Event    `json:"history"`
	Deleted     bool       `json:"deleted,omitempty"`
}

// Push is one SSE message. A non-nil Snapshot replaces the whole board.
// Otherwise Job is a single update.
type Push struct {
	Job      JobView
	Snapshot *Snapshot
}

// Snapshot is the full board. SSE sends it on connect.
// Abandon windows are seconds so a client can derive abandoned without waiting
// for the next event. Zero means that category is disabled.
type Snapshot struct {
	Now                        time.Time `json:"now"`
	Jobs                       []JobView `json:"jobs"`
	Events                     []Event   `json:"events"`
	AbandonAfterSeconds        int64     `json:"abandon_after_seconds"`
	AbandonWaitingAfterSeconds int64     `json:"abandon_waiting_after_seconds"`
}

// Options configures Open. Now, when set, replaces time.Now (tests).
// A nil abandon window keeps the default (24h running, 168h queued/blocked).
// A pointer to 0 disables that category. A zero Options value does not.
type Options struct {
	Now                 func() time.Time
	AbandonAfter        *time.Duration
	AbandonWaitingAfter *time.Duration
}

// abandonPolicy is the read-time silence window. Zero disables that category.
type abandonPolicy struct {
	after   time.Duration
	waiting time.Duration
}

type jobState struct {
	agent      string
	jobID      string
	title      string
	state      string
	progress   int
	link       string
	note       string
	ttl        int
	seq        int64
	hasSeq     bool
	startedAt  time.Time
	updatedAt  time.Time
	finishedAt *time.Time
	via        string
	ip         string
	country    string
	receivedAt string
	history    []Event
}

// Store is safe for concurrent use.
type Store struct {
	mu     sync.Mutex
	path   string
	file   *os.File
	jobs   map[string]*jobState
	recent []Event
	// byAgent keeps every event replayed from the log, plus later updates.
	// recent and per-job history stay capped; this list does not. Compaction
	// may drop older log lines on a future Open, which is the only trim.
	byAgent map[string][]Event
	seenN   map[int64]struct{}
	lines   int
	nextN   int64
	nextSub int
	subs    map[int]chan Push
	clock   func() time.Time
	// Set once in OpenWith. Abandoned is derived from these; it is not logged.
	abandonAfter        time.Duration
	abandonWaitingAfter time.Duration
}

var validStates = map[string]struct{}{
	"queued":  {},
	"running": {},
	"blocked": {},
	"done":    {},
	"failed":  {},
}

// Validate checks fields that do not depend on stored jobs.
// An empty state is allowed here. Apply and ValidateBatch reject it when
// the job does not already exist. Invalid non-empty states are rejected.
func Validate(u Update) error {
	if err := validateID(u.Agent, "agent"); err != nil {
		return err
	}
	if err := validateID(u.JobID, "job_id"); err != nil {
		return err
	}
	if u.State != "" {
		if _, ok := validStates[u.State]; !ok {
			return invalid("state must be queued, running, blocked, done, or failed")
		}
	}
	if u.Progress != nil && (*u.Progress < 0 || *u.Progress > 100) {
		return invalid("progress must be an integer from 0 to 100")
	}
	if u.TTL != nil && *u.TTL < 0 {
		return invalid("ttl must be >= 0")
	}
	return nil
}

func validateID(v, field string) error {
	if strings.TrimSpace(v) == "" {
		return invalid(field + " is required")
	}
	if strings.ContainsAny(v, "/\\\x00") {
		return invalid(field + " must not contain / or \\")
	}
	return nil
}

func (s *Store) now() time.Time {
	if s.clock != nil {
		return utc(s.clock())
	}
	return utc(time.Now())
}

func utc(t time.Time) time.Time {
	return t.UTC().Round(0)
}

// Apply validates, appends, and merges one update. applied is false when an
// explicit seq is older than or equal to the seq already stored.
func (s *Store) Apply(u Update) (bool, error) {
	return s.ApplyAt(u, time.Time{})
}

// ApplyAt is Apply with an explicit timestamp. A zero ts uses the store clock.
// Relay sync passes the remote event time so elapsed and stalled stay honest.
func (s *Store) ApplyAt(u Update, ts time.Time) (bool, error) {
	if err := Validate(u); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireState(u, nil); err != nil {
		return false, err
	}
	if ts.IsZero() {
		ts = s.now()
	} else {
		ts = utc(ts)
	}
	n := s.nextN + 1
	if err := s.appendRecord(recordFromUpdate(u, n, ts)); err != nil {
		return false, err
	}
	s.nextN = n
	applied := s.absorb(u, ts, n)
	if applied {
		if job, ok := s.jobs[jobKey(u.Agent, u.JobID)]; ok {
			s.broadcast(job.view(s.now(), s.policy()))
		}
	}
	return applied, nil
}

// Delete removes a job and appends a tombstone so replay stays deleted.
// The bool is false when the job was not present.
func (s *Store) Delete(agent, jobID string) (bool, error) {
	if err := validateID(agent, "agent"); err != nil {
		return false, err
	}
	if err := validateID(jobID, "job_id"); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[jobKey(agent, jobID)]; !ok {
		return false, nil
	}
	ts := s.now()
	n := s.nextN + 1
	rec := logRecord{Kind: "delete", N: n, TS: ts, Agent: agent, JobID: jobID}
	if err := s.appendRecord(rec); err != nil {
		return false, err
	}
	s.nextN = n
	s.removeJob(agent, jobID, ts, n, true)
	return true, nil
}

// BoardSection is the board group for v: attention, progress, queued, or done.
// Empty means the job would not appear. Stalled and abandoned running jobs are
// attention, as are abandoned queued and blocked jobs. This matches the board.
func BoardSection(v JobView) string {
	if v.State != "done" && v.State != "failed" && v.Abandoned {
		return "attention"
	}
	if v.State == "running" && v.Stalled {
		return "attention"
	}
	switch v.State {
	case "failed", "blocked":
		return "attention"
	case "running":
		return "progress"
	case "queued":
		return "queued"
	case "done":
		return "done"
	default:
		return ""
	}
}

// ClearSection appends a delete tombstone for every job currently in section.
// It does not truncate the log or write a backup. Other jobs and their history
// stay. An invalid section is a validation error and changes nothing. When
// nothing matches, it publishes no snapshot. Otherwise subscribers get one
// snapshot of the remaining board. A later Apply for a cleared job_id can
// bring that job back, same as Delete.
func (s *Store) ClearSection(section string) (int, error) {
	switch section {
	case "attention", "progress", "queued", "done":
	default:
		return 0, invalid(`section must be "attention", "progress", "queued", or "done"`)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return 0, fmt.Errorf("store is closed")
	}
	now := s.now()
	var matched []JobView
	for _, job := range s.listLocked(now) {
		if BoardSection(job) == section {
			matched = append(matched, job)
		}
	}
	if len(matched) == 0 {
		return 0, nil
	}
	n := 0
	var appendErr error
	for _, job := range matched {
		id := s.nextN + 1
		rec := logRecord{Kind: "delete", N: id, TS: now, Agent: job.Agent, JobID: job.JobID}
		if err := s.appendRecord(rec); err != nil {
			appendErr = err
			break
		}
		s.nextN = id
		s.removeJob(job.Agent, job.JobID, now, id, false)
		n++
	}
	if n > 0 {
		snap := s.snapshotLocked()
		s.send(Push{Snapshot: &snap})
	}
	return n, appendErr
}

// Clear deletes every job and all history. It fsyncs the live log, copies it to
// jobs-backup-<unix>.jsonl in the data directory, then truncates the log so a
// later Open replays an empty board. Subscribers receive a fresh snapshot.
// The backup path is absolute. n is the number of jobs removed.
func (s *Store) Clear() (n int, backup string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return 0, "", fmt.Errorf("store is closed")
	}
	if err := s.file.Sync(); err != nil {
		return 0, "", err
	}
	dest, err := uniqueBackup(filepath.Dir(s.path), s.now().Unix())
	if err != nil {
		return 0, "", err
	}
	if err := copyFile(dest, s.path); err != nil {
		return 0, "", err
	}
	n = len(s.jobs)
	if err := s.file.Truncate(0); err != nil {
		return 0, "", err
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return 0, "", err
	}
	if err := s.file.Sync(); err != nil {
		return 0, "", err
	}
	s.jobs = make(map[string]*jobState)
	s.recent = nil
	s.byAgent = make(map[string][]Event)
	s.seenN = make(map[int64]struct{})
	s.lines = 0
	s.nextN = 0
	snap := s.snapshotLocked()
	s.send(Push{Snapshot: &snap})
	abs, err := filepath.Abs(dest)
	if err != nil {
		return n, dest, nil
	}
	return n, abs, nil
}

// absorb merges u into current state. A missing seq is treated as newer.
// An explicit seq <= the stored seq is kept in history and does not merge.
func (s *Store) absorb(u Update, ts time.Time, n int64) bool {
	ts = utc(ts)
	key := jobKey(u.Agent, u.JobID)
	job := s.jobs[key]
	applied := job == nil || u.Seq == nil || !job.hasSeq || *u.Seq > job.seq
	if job == nil {
		job = &jobState{
			agent:     u.Agent,
			jobID:     u.JobID,
			ttl:       defaultTTL,
			startedAt: ts,
		}
		s.jobs[key] = job
		applied = true
	}
	ev := eventFromUpdate(u, ts, n, applied)
	if applied {
		job.merge(u, ts)
	}
	job.history = append(job.history, ev)
	job.history = trimTail(job.history, maxHist)
	s.pushRecent(ev)
	return applied
}

func (j *jobState) merge(u Update, ts time.Time) {
	if u.Title != nil {
		j.title = *u.Title
	}
	if u.Link != nil {
		j.link = *u.Link
	}
	if u.Progress != nil {
		j.progress = *u.Progress
	}
	if u.Note != nil {
		j.note = *u.Note
	}
	if u.TTL != nil {
		j.ttl = *u.TTL
	}
	if u.State != "" {
		j.state = u.State
		if u.State == "done" || u.State == "failed" {
			if j.finishedAt == nil {
				t := ts
				j.finishedAt = &t
			}
		} else {
			j.finishedAt = nil
		}
	}
	if u.Seq != nil {
		j.seq = *u.Seq
		j.hasSeq = true
	}
	j.updatedAt = ts
	// Latest applied update wins, including a local post that clears a relay stamp.
	j.via = u.Via
	j.ip = u.IP
	j.country = u.Country
	j.receivedAt = u.ReceivedAt
}

func (s *Store) removeJob(agent, jobID string, ts time.Time, n int64, notify bool) {
	delete(s.jobs, jobKey(agent, jobID))
	ev := Event{
		N:       n,
		Agent:   agent,
		JobID:   jobID,
		TS:      utc(ts),
		Applied: true,
		Kind:    "delete",
	}
	s.pushRecent(ev)
	if notify {
		s.broadcast(JobView{
			Agent:   agent,
			JobID:   jobID,
			Deleted: true,
			History: []Event{ev},
		})
	}
}

func (s *Store) pushRecent(ev Event) {
	s.recent = append(s.recent, ev)
	s.recent = trimTail(s.recent, maxRecent)
	s.remember(ev)
}

// remember appends ev to the per-agent log. Event n is unique, so a snap
// replay followed by the recent feed does not store the same row twice.
func (s *Store) remember(ev Event) {
	if s.byAgent == nil {
		s.byAgent = make(map[string][]Event)
	}
	if s.seenN == nil {
		s.seenN = make(map[int64]struct{})
	}
	if ev.N > 0 {
		if _, ok := s.seenN[ev.N]; ok {
			return
		}
		s.seenN[ev.N] = struct{}{}
	}
	s.byAgent[ev.Agent] = append(s.byAgent[ev.Agent], ev)
}

// requireState rejects a new job that omitted state. pending holds jobs an
// earlier item in the same batch would create. Caller holds s.mu, except
// during single-threaded replay.
func (s *Store) requireState(u Update, pending map[string]struct{}) error {
	if u.State != "" {
		return nil
	}
	key := jobKey(u.Agent, u.JobID)
	if _, ok := s.jobs[key]; ok {
		return nil
	}
	if _, ok := pending[key]; ok {
		return nil
	}
	return invalid("state must be queued, running, blocked, done, or failed")
}

// ValidateBatch checks a batch without writing. An omitted state is valid
// only for a job that already exists or that an earlier item creates.
func (s *Store) ValidateBatch(updates []Update) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := make(map[string]struct{}, len(updates))
	for _, u := range updates {
		if err := Validate(u); err != nil {
			return err
		}
		if err := s.requireState(u, pending); err != nil {
			return err
		}
		if u.State == "" {
			continue
		}
		if _, ok := s.jobs[jobKey(u.Agent, u.JobID)]; ok {
			continue
		}
		pending[jobKey(u.Agent, u.JobID)] = struct{}{}
	}
	return nil
}

func eventFromUpdate(u Update, ts time.Time, n int64, applied bool) Event {
	ev := Event{
		N:       n,
		Agent:   u.Agent,
		JobID:   u.JobID,
		State:   u.State,
		TS:      ts,
		Applied: applied,
		Kind:    "update",
	}
	if u.Title != nil {
		ev.Title = *u.Title
	}
	if u.Note != nil {
		ev.Note = *u.Note
	}
	if u.Link != nil {
		ev.Link = *u.Link
	}
	if u.Progress != nil {
		p := *u.Progress
		ev.Progress = &p
	}
	if u.Seq != nil {
		seq := *u.Seq
		ev.Seq = &seq
	}
	ev.Via = u.Via
	ev.IP = u.IP
	ev.Country = u.Country
	ev.ReceivedAt = u.ReceivedAt
	return ev
}

// List returns jobs sorted by agent, then updated_at descending.
func (s *Store) List() (time.Time, []JobView) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	return now, s.listLocked(now)
}

// Get returns one job.
func (s *Store) Get(agent, jobID string) (JobView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobKey(agent, jobID)]
	if !ok {
		return JobView{}, false
	}
	return job.view(s.now(), s.policy()), true
}

// AbandonPolicySeconds reports the read-time abandon windows in whole seconds.
// Zero means that category is disabled. The windows are fixed at Open.
func (s *Store) AbandonPolicySeconds() (after, waiting int64) {
	return durationSeconds(s.abandonAfter), durationSeconds(s.abandonWaitingAfter)
}

func (s *Store) policy() abandonPolicy {
	return abandonPolicy{after: s.abandonAfter, waiting: s.abandonWaitingAfter}
}

// AgentEvents returns every remembered event for agent, newest first.
// The list is the log replay plus later updates. It is not capped at
// maxHist or maxRecent. A later compaction may shorten what the next
// Open can replay; this process keeps what it already read.
func (s *Store) AgentEvents(agent string) (time.Time, []Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	src := s.byAgent[agent]
	out := make([]Event, len(src))
	copy(out, src)
	sort.Slice(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		return out[i].TS.After(out[j].TS)
	})
	return now, out
}

// Events returns the global feed, newest first. Non-positive limit yields an empty list.
func (s *Store) Events(limit int) (time.Time, []Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if limit <= 0 {
		return now, []Event{}
	}
	if limit > maxRecent {
		limit = maxRecent
	}
	return now, s.eventsLocked(limit)
}

// Subscribe registers an SSE client. The snapshot is taken under the same lock
// that publishes, so the client cannot miss an update between snapshot and stream.
// Cancel drops the client. A full buffer drops that client and never blocks ingest.
func (s *Store) Subscribe() (Snapshot, <-chan Push, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextSub
	s.nextSub++
	ch := make(chan Push, 32)
	if s.subs == nil {
		s.subs = make(map[int]chan Push)
	}
	s.subs[id] = ch
	snap := s.snapshotLocked()
	var once sync.Once
	cancel := func() {
		once.Do(func() { s.removeSub(id) })
	}
	return snap, ch, cancel
}

func (s *Store) removeSub(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.subs[id]
	if !ok {
		return
	}
	delete(s.subs, id)
	close(ch)
}

func (s *Store) broadcast(v JobView) {
	s.send(Push{Job: v})
}

func (s *Store) send(p Push) {
	for id, ch := range s.subs {
		select {
		case ch <- p:
		default:
			delete(s.subs, id)
			close(ch)
		}
	}
}

func (s *Store) snapshotLocked() Snapshot {
	now := s.now()
	after, waiting := s.AbandonPolicySeconds()
	return Snapshot{
		Now:                        now,
		Jobs:                       s.listLocked(now),
		Events:                     s.eventsLocked(maxRecent),
		AbandonAfterSeconds:        after,
		AbandonWaitingAfterSeconds: waiting,
	}
}

func (s *Store) listLocked(now time.Time) []JobView {
	jobs := make([]JobView, 0, len(s.jobs))
	pol := s.policy()
	for _, job := range s.jobs {
		jobs = append(jobs, job.view(now, pol))
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].Agent != jobs[j].Agent {
			return jobs[i].Agent < jobs[j].Agent
		}
		if !jobs[i].UpdatedAt.Equal(jobs[j].UpdatedAt) {
			return jobs[i].UpdatedAt.After(jobs[j].UpdatedAt)
		}
		return jobs[i].JobID < jobs[j].JobID
	})
	return jobs
}

func (s *Store) eventsLocked(limit int) []Event {
	if limit > len(s.recent) {
		limit = len(s.recent)
	}
	out := make([]Event, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, s.recent[len(s.recent)-1-i])
	}
	return out
}

func (j *jobState) view(now time.Time, pol abandonPolicy) JobView {
	hist := make([]Event, len(j.history))
	copy(hist, j.history)
	abandoned, abandonedAt := j.abandoned(now, pol)
	v := JobView{
		Agent:          j.agent,
		JobID:          j.jobID,
		Title:          j.title,
		State:          j.state,
		Progress:       j.progress,
		Link:           j.link,
		TTL:            j.ttl,
		Note:           j.note,
		Via:            j.via,
		IP:             j.ip,
		Country:        j.country,
		ReceivedAt:     j.receivedAt,
		StartedAt:      j.startedAt,
		UpdatedAt:      j.updatedAt,
		ElapsedSeconds: j.elapsed(now),
		Stalled:        j.stalled(now) && !abandoned,
		Abandoned:      abandoned,
		History:        hist,
	}
	if abandoned {
		t := abandonedAt
		v.AbandonedAt = &t
	}
	if j.hasSeq {
		seq := j.seq
		v.Seq = &seq
	}
	if j.finishedAt != nil {
		t := *j.finishedAt
		v.FinishedAt = &t
	}
	return v
}

func (j *jobState) elapsed(now time.Time) int64 {
	end := now
	if j.finishedAt != nil && (j.state == "done" || j.state == "failed") {
		end = *j.finishedAt
	}
	sec := int64(end.Sub(j.startedAt) / time.Second)
	if sec < 0 {
		return 0
	}
	return sec
}

func (j *jobState) stalled(now time.Time) bool {
	// Queued work has not started. Blocked means a person is holding the job.
	// Done and failed are finished. Only running work can go stale.
	if j.state != "running" {
		return false
	}
	age := now.Sub(j.updatedAt)
	if age <= 0 {
		return false
	}
	if j.ttl <= 0 {
		return true
	}
	limit := time.Duration(j.ttl) * time.Second
	if limit/time.Second != time.Duration(j.ttl) {
		return false
	}
	return age > limit
}

// abandoned reports whether silence has crossed the read-time threshold.
// The timestamp is updated_at plus that threshold. Nothing is written to the log.
// Done and failed never qualify. A zero window disables that category.
func (j *jobState) abandoned(now time.Time, pol abandonPolicy) (bool, time.Time) {
	limit, ok := j.abandonLimit(pol)
	if !ok {
		return false, time.Time{}
	}
	age := now.Sub(j.updatedAt)
	// Equal to the threshold is not yet abandoned, same as stalled at ttl.
	if age <= limit {
		return false, time.Time{}
	}
	return true, j.updatedAt.Add(limit)
}

func (j *jobState) abandonLimit(pol abandonPolicy) (time.Duration, bool) {
	switch j.state {
	case "running":
		return runningAbandonLimit(j.ttl, pol.after)
	case "queued", "blocked":
		if pol.waiting <= 0 {
			return 0, false
		}
		return pol.waiting, true
	default:
		return 0, false
	}
}

// runningAbandonLimit is max(after, 4*ttl) once the job can already be stalled.
// ttl overflow matches stalled: the job is not abandoned.
func runningAbandonLimit(ttl int, after time.Duration) (time.Duration, bool) {
	if after <= 0 {
		return 0, false
	}
	four, ok := scaledTTL(ttl, abandonTTLFactor)
	if !ok {
		return 0, false
	}
	if four > after {
		return four, true
	}
	return after, true
}

// scaledTTL returns factor * ttl seconds. ok is false when the multiply overflows,
// the same caution stalled uses for ttl seconds.
func scaledTTL(ttl, factor int) (time.Duration, bool) {
	if ttl <= 0 {
		return 0, true
	}
	if factor <= 0 {
		return 0, true
	}
	if ttl > int(^uint(0)>>1)/factor {
		return 0, false
	}
	secs := time.Duration(ttl * factor)
	d := secs * time.Second
	if d/time.Second != secs {
		return 0, false
	}
	return d, true
}

func resolveDuration(set *time.Duration, fallback time.Duration) time.Duration {
	if set == nil {
		return fallback
	}
	if *set < 0 {
		return 0
	}
	return *set
}

func durationSeconds(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return int64(d / time.Second)
}

func (j *jobState) snap() *snapJob {
	hist := make([]Event, len(j.history))
	copy(hist, j.history)
	out := &snapJob{
		Agent:      j.agent,
		JobID:      j.jobID,
		Title:      j.title,
		State:      j.state,
		Progress:   j.progress,
		Link:       j.link,
		TTL:        j.ttl,
		Note:       j.note,
		Via:        j.via,
		IP:         j.ip,
		Country:    j.country,
		ReceivedAt: j.receivedAt,
		StartedAt:  j.startedAt,
		UpdatedAt:  j.updatedAt,
		History:    hist,
	}
	if j.hasSeq {
		seq := j.seq
		out.Seq = &seq
	}
	if j.finishedAt != nil {
		t := *j.finishedAt
		out.FinishedAt = &t
	}
	return out
}

func jobKey(agent, jobID string) string {
	return agent + "\x00" + jobID
}

func trimTail[T any](in []T, n int) []T {
	if len(in) <= n {
		return in
	}
	out := make([]T, n)
	copy(out, in[len(in)-n:])
	return out
}
