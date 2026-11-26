package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const logName = "events.jsonl"

// logRecord is one JSONL line. Updates store the raw fields so replay applies
// the same seq and merge rules. Snapshots store the full merged job so compaction
// can drop older lines without losing fields set before the history window.
type logRecord struct {
	Kind       string    `json:"kind"`
	N          int64     `json:"n,omitempty"`
	TS         time.Time `json:"ts,omitempty"`
	Agent      string    `json:"agent,omitempty"`
	JobID      string    `json:"job_id,omitempty"`
	Title      *string   `json:"title,omitempty"`
	State      string    `json:"state,omitempty"`
	Progress   *int      `json:"progress,omitempty"`
	Link       *string   `json:"link,omitempty"`
	Seq        *int64    `json:"seq,omitempty"`
	TTL        *int      `json:"ttl,omitempty"`
	Note       *string   `json:"note,omitempty"`
	Via        string    `json:"via,omitempty"`
	IP         string    `json:"ip,omitempty"`
	Country    string    `json:"country,omitempty"`
	ReceivedAt string    `json:"received_at,omitempty"`
	Job        *snapJob  `json:"job,omitempty"`
	Events     []Event   `json:"events,omitempty"`
}

type snapJob struct {
	Agent      string     `json:"agent"`
	JobID      string     `json:"job_id"`
	Title      string     `json:"title"`
	State      string     `json:"state"`
	Progress   int        `json:"progress"`
	Link       string     `json:"link,omitempty"`
	Seq        *int64     `json:"seq,omitempty"`
	TTL        int        `json:"ttl"`
	Note       string     `json:"note,omitempty"`
	Via        string     `json:"via,omitempty"`
	IP         string     `json:"ip,omitempty"`
	Country    string     `json:"country,omitempty"`
	ReceivedAt string     `json:"received_at,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	History    []Event    `json:"history"`
}

// Open loads dir/events.jsonl, creating the directory if needed.
func Open(dir string) (*Store, error) {
	return OpenWith(dir, Options{})
}

// OpenWith is Open with Options. A zero Options value keeps the default clock
// and the default abandon windows (24h running, 168h queued or blocked).
func OpenWith(dir string, opt Options) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("data dir is empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, logName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		unlockFile(f)
		f.Close()
		return nil, err
	}
	s := &Store{
		path:                path,
		file:                f,
		jobs:                make(map[string]*jobState),
		byAgent:             make(map[string][]Event),
		seenN:               make(map[int64]struct{}),
		subs:                make(map[int]chan Push),
		clock:               opt.Now,
		abandonAfter:        resolveDuration(opt.AbandonAfter, defaultAbandonAfter),
		abandonWaitingAfter: resolveDuration(opt.AbandonWaitingAfter, defaultAbandonWaitingAfter),
	}
	if err := s.replay(); err != nil {
		s.closeFile()
		return nil, err
	}
	if err := s.maybeCompact(); err != nil {
		s.closeFile()
		return nil, err
	}
	return s, nil
}

// Close fsyncs the log and releases the directory lock.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, ch := range s.subs {
		delete(s.subs, id)
		close(ch)
	}
	return s.closeFile()
}

func (s *Store) closeFile() error {
	if s.file == nil {
		return nil
	}
	errSync := s.file.Sync()
	errUnlock := unlockFile(s.file)
	errClose := s.file.Close()
	s.file = nil
	if errSync != nil {
		return errSync
	}
	if errUnlock != nil {
		return errUnlock
	}
	return errClose
}

func (s *Store) replay() error {
	sc := bufio.NewScanner(s.file)
	sc.Buffer(make([]byte, 64*1024), 2<<20)
	lineNum := 0
	for sc.Scan() {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		lineNum++
		s.lines++
		if err := s.replayLine(raw); err != nil {
			return fmt.Errorf("events.jsonl line %d: %w", lineNum, err)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read events.jsonl: %w", err)
	}
	return nil
}

func (s *Store) replayLine(raw []byte) error {
	var rec logRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return err
	}
	switch rec.Kind {
	case "update":
		return s.replayUpdate(rec)
	case "delete":
		s.replayDelete(rec)
		return nil
	case "snap":
		if rec.Job == nil {
			return fmt.Errorf("snap missing job")
		}
		s.restore(*rec.Job)
		return nil
	case "recent":
		s.restoreRecent(rec.Events)
		return nil
	default:
		return fmt.Errorf("unknown record kind %q", rec.Kind)
	}
}

func (s *Store) replayUpdate(rec logRecord) error {
	u := rec.toUpdate()
	if err := Validate(u); err != nil {
		return err
	}
	if err := s.requireState(u, nil); err != nil {
		return err
	}
	if rec.N > s.nextN {
		s.nextN = rec.N
	}
	s.absorb(u, rec.TS, rec.N)
	return nil
}

func (s *Store) replayDelete(rec logRecord) {
	if rec.N > s.nextN {
		s.nextN = rec.N
	}
	if _, ok := s.jobs[jobKey(rec.Agent, rec.JobID)]; !ok {
		return
	}
	s.removeJob(rec.Agent, rec.JobID, rec.TS, rec.N, false)
}

func (s *Store) restore(snap snapJob) {
	hist := make([]Event, len(snap.History))
	copy(hist, snap.History)
	for i := range hist {
		hist[i].TS = utc(hist[i].TS)
		if hist[i].N > s.nextN {
			s.nextN = hist[i].N
		}
		s.remember(hist[i])
	}
	job := &jobState{
		agent:      snap.Agent,
		jobID:      snap.JobID,
		title:      snap.Title,
		state:      snap.State,
		progress:   snap.Progress,
		link:       snap.Link,
		ttl:        snap.TTL,
		note:       snap.Note,
		via:        snap.Via,
		ip:         snap.IP,
		country:    snap.Country,
		receivedAt: snap.ReceivedAt,
		hasSeq:     snap.Seq != nil,
		startedAt:  utc(snap.StartedAt),
		updatedAt:  utc(snap.UpdatedAt),
		history:    hist,
	}
	if snap.Seq != nil {
		job.seq = *snap.Seq
	}
	if snap.FinishedAt != nil {
		t := utc(*snap.FinishedAt)
		job.finishedAt = &t
	}
	s.jobs[jobKey(job.agent, job.jobID)] = job
}

func (s *Store) restoreRecent(events []Event) {
	out := make([]Event, 0, len(events))
	for _, ev := range events {
		ev.TS = utc(ev.TS)
		if ev.N > s.nextN {
			s.nextN = ev.N
		}
		out = append(out, ev)
		s.remember(ev)
	}
	s.recent = trimTail(out, maxRecent)
}

func (s *Store) maybeCompact() error {
	limit := len(s.jobs)*maxHist + compactSlack
	if s.lines <= limit {
		return nil
	}
	return s.compact()
}

func (s *Store) compact() error {
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	lines, werr := s.writeSnapshot(f)
	if werr != nil {
		f.Close()
		os.Remove(tmp)
		return werr
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if s.file != nil {
		_ = unlockFile(s.file)
		if err := s.file.Close(); err != nil {
			return err
		}
		s.file = nil
	}
	if err := os.Rename(tmp, s.path); err != nil {
		if rerr := s.reopenAppend(); rerr != nil {
			return fmt.Errorf("rename compacted log: %w (reopen failed: %v)", err, rerr)
		}
		return err
	}
	_ = syncDir(filepath.Dir(s.path))
	if err := s.reopenAppend(); err != nil {
		return err
	}
	s.lines = lines
	return nil
}

func (s *Store) writeSnapshot(f *os.File) (int, error) {
	keys := make([]string, 0, len(s.jobs))
	for k := range s.jobs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := 0
	for _, k := range keys {
		rec := logRecord{Kind: "snap", Job: s.jobs[k].snap()}
		if err := writeJSONLine(f, rec); err != nil {
			return 0, err
		}
		lines++
	}
	recent := append([]Event(nil), s.recent...)
	if err := writeJSONLine(f, logRecord{Kind: "recent", Events: recent}); err != nil {
		return 0, err
	}
	return lines + 1, nil
}

func (s *Store) reopenAppend() error {
	f, err := os.OpenFile(s.path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		_ = unlockFile(f)
		f.Close()
		return err
	}
	s.file = f
	return nil
}

func (s *Store) appendRecord(rec logRecord) error {
	data, err := marshalRecord(rec)
	if err != nil {
		return err
	}
	if _, err := s.file.Write(data); err != nil {
		return err
	}
	if err := s.file.Sync(); err != nil {
		return err
	}
	s.lines++
	return nil
}

func marshalRecord(rec logRecord) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rec); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeJSONLine(w io.Writer, rec logRecord) error {
	data, err := marshalRecord(rec)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func uniqueBackup(dir string, unix int64) (string, error) {
	candidates := []string{fmt.Sprintf("jobs-backup-%d.jsonl", unix)}
	for i := 2; i < 1000; i++ {
		candidates = append(candidates, fmt.Sprintf("jobs-backup-%d-%d.jsonl", unix, i))
	}
	for _, name := range candidates {
		path := filepath.Join(dir, name)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if err := f.Close(); err != nil {
			return "", err
		}
		return path, nil
	}
	return "", fmt.Errorf("backup name exhausted")
}

func copyFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func recordFromUpdate(u Update, n int64, ts time.Time) logRecord {
	return logRecord{
		Kind:       "update",
		N:          n,
		TS:         ts,
		Agent:      u.Agent,
		JobID:      u.JobID,
		Title:      u.Title,
		State:      u.State,
		Progress:   u.Progress,
		Link:       u.Link,
		Seq:        u.Seq,
		TTL:        u.TTL,
		Note:       u.Note,
		Via:        u.Via,
		IP:         u.IP,
		Country:    u.Country,
		ReceivedAt: u.ReceivedAt,
	}
}

func (r logRecord) toUpdate() Update {
	return Update{
		Agent:      r.Agent,
		JobID:      r.JobID,
		Title:      r.Title,
		State:      r.State,
		Progress:   r.Progress,
		Link:       r.Link,
		Seq:        r.Seq,
		TTL:        r.TTL,
		Note:       r.Note,
		Via:        r.Via,
		IP:         r.IP,
		Country:    r.Country,
		ReceivedAt: r.ReceivedAt,
	}
}
