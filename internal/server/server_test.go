package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kirillzubovsky/statuspan/internal/relay"
	"github.com/kirillzubovsky/statuspan/internal/store"
)

func newTestServer(t *testing.T) (*store.Store, *httptest.Server) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ts := httptest.NewServer(New(st).Handler())
	t.Cleanup(ts.Close)
	return st, ts
}

func postRaw(t *testing.T, base, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(base+"/api/jobs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPostValidation(t *testing.T) {
	_, ts := newTestServer(t)
	cases := []struct {
		body string
		code int
	}{
		{`{}`, 400},
		{`{"agent":"a","job_id":"j","state":"nope"}`, 400},
		{`{"agent":"a","job_id":"j","state":"Running"}`, 400},
		{`{"agent":"a","job_id":"j","state":"running","progress":101}`, 400},
		{`{"agent":"a","job_id":"j","state":"running","progress":-1}`, 400},
		{`{"agent":"","job_id":"j","state":"running"}`, 400},
		{`{"agent":"a","job_id":"","state":"running"}`, 400},
		{`{"agent":"a","job_id":"j"}`, 400},
		{`{"agent":"a/b","job_id":"j","state":"running"}`, 400},
		{`{"agent":"a","job_id":"j","state":"running","ttl":-5}`, 400},
		{`{"agent":"a","job_id":"j","state":"abandoned"}`, 400},
		{`not json`, 400},
		{``, 400},
		{`null`, 400},
	}
	for _, tc := range cases {
		resp := postRaw(t, ts.URL, tc.body)
		if resp.StatusCode != tc.code {
			t.Fatalf("body %s: status %d (%s)", tc.body, resp.StatusCode, readBody(t, resp))
		}
	}

	resp := postRaw(t, ts.URL, `{"agent":"a","job_id":"j","state":"running","progress":0}`)
	if resp.StatusCode != 200 {
		t.Fatalf("progress 0: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp = postRaw(t, ts.URL, `{"agent":"a","job_id":"j2","state":"failed","progress":100}`)
	if resp.StatusCode != 200 {
		t.Fatalf("progress 100: %d %s", resp.StatusCode, readBody(t, resp))
	}
}

func TestBatchIsAtomicOnValidation(t *testing.T) {
	_, ts := newTestServer(t)
	resp := postRaw(t, ts.URL, `[{"agent":"a","job_id":"good","state":"queued"},{"agent":"a","job_id":"bad","state":"nope"}]`)
	if resp.StatusCode != 400 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	body := getJSON(t, ts.URL+"/api/jobs")
	if strings.Contains(body, `"job_id":"good"`) || strings.Contains(body, `"job_id":"bad"`) {
		t.Fatalf("invalid batch was applied: %s", body)
	}

	resp = postRaw(t, ts.URL, `[{"agent":"a","job_id":"j1","state":"queued","title":"One"},{"agent":"b","job_id":"j2","state":"running","progress":5}]`)
	if resp.StatusCode != 200 || !strings.Contains(readBody(t, resp), `"applied":true`) {
		t.Fatalf("batch status")
	}
	body = getJSON(t, ts.URL+"/api/jobs")
	if !strings.Contains(body, `"job_id":"j1"`) || !strings.Contains(body, `"job_id":"j2"`) {
		t.Fatalf("batch missing jobs: %s", body)
	}
}

func TestHTTPRoundTripSeqAndMerge(t *testing.T) {
	_, ts := newTestServer(t)
	resp := postRaw(t, ts.URL, `{"agent":"my bot","job_id":"build 1","title":"Build v1","state":"running","progress":40,"link":"http://example.test/a","ttl":600,"seq":5}`)
	if resp.StatusCode != 200 || !strings.Contains(readBody(t, resp), `"applied":true`) {
		t.Fatalf("create failed: %d", resp.StatusCode)
	}

	resp = postRaw(t, ts.URL, `{"agent":"my bot","job_id":"build 1","title":"older","state":"failed","progress":1,"seq":4}`)
	got := readBody(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(got, `"applied":false`) {
		t.Fatalf("older seq: %d %s", resp.StatusCode, got)
	}

	resp = postRaw(t, ts.URL, `{"agent":"my bot","job_id":"build 1","state":"blocked","note":"waiting on ci"}`)
	if resp.StatusCode != 200 || !strings.Contains(readBody(t, resp), `"applied":true`) {
		t.Fatal("merge update failed")
	}

	rawList := getJSON(t, ts.URL+"/api/jobs")
	if !strings.Contains(rawList, `"abandon_after_seconds":86400`) || !strings.Contains(rawList, `"abandon_waiting_after_seconds":604800`) {
		t.Fatalf("list policy: %s", rawList)
	}
	if !strings.Contains(rawList, `"abandoned":false`) || strings.Contains(rawList, `"abandoned_at"`) {
		t.Fatalf("fresh job abandon fields: %s", rawList)
	}
	var list struct {
		Jobs []store.JobView `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(rawList), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Jobs) != 1 {
		t.Fatalf("jobs = %d", len(list.Jobs))
	}
	job := list.Jobs[0]
	if job.Title != "Build v1" || job.Progress != 40 || job.Link != "http://example.test/a" || job.Note != "waiting on ci" || job.State != "blocked" {
		t.Fatalf("merged job: %+v", job)
	}
	if job.Stalled {
		t.Fatal("fresh job stalled")
	}
	if job.TTL != 600 {
		t.Fatalf("ttl %d", job.TTL)
	}
	if job.Seq == nil || *job.Seq != 5 {
		t.Fatalf("seq %v", job.Seq)
	}

	path := ts.URL + "/api/jobs/" + url.PathEscape("my bot") + "/" + url.PathEscape("build 1")
	oneRaw := getJSON(t, path)
	if !strings.Contains(oneRaw, `"abandoned":false`) {
		t.Fatalf("get one abandon: %s", oneRaw)
	}
	var one store.JobView
	if err := json.Unmarshal([]byte(oneRaw), &one); err != nil {
		t.Fatal(err)
	}
	if one.JobID != "build 1" || one.Title != job.Title {
		t.Fatalf("get one: %+v", one)
	}

	missing, err := http.Get(ts.URL + "/api/jobs/nope/missing")
	if err != nil {
		t.Fatal(err)
	}
	defer missing.Body.Close()
	if missing.StatusCode != 404 {
		t.Fatalf("missing status %d", missing.StatusCode)
	}

	events := getJSON(t, ts.URL+"/api/events?limit=2")
	if !strings.Contains(events, `"note":"waiting on ci"`) {
		t.Fatalf("events: %s", events)
	}

	health, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer health.Body.Close()
	hb, _ := io.ReadAll(health.Body)
	if health.StatusCode != 200 {
		t.Fatalf("health %d %q", health.StatusCode, hb)
	}
	var healthBody struct {
		OK    bool `json:"ok"`
		Relay struct {
			Configured bool   `json:"configured"`
			Cursor     int64  `json:"cursor"`
			LastError  string `json:"last_error"`
		} `json:"relay"`
	}
	if err := json.Unmarshal(hb, &healthBody); err != nil {
		t.Fatal(err)
	}
	if !healthBody.OK || healthBody.Relay.Configured || healthBody.Relay.Cursor != 0 || healthBody.Relay.LastError != "" {
		t.Fatalf("health %s", hb)
	}
	if !bytes.Contains(hb, []byte(`"last_ok":null`)) {
		t.Fatalf("health missing last_ok: %s", hb)
	}

	index, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer index.Body.Close()
	ib, _ := io.ReadAll(index.Body)
	if index.StatusCode != 200 || !bytes.Contains(ib, []byte("StatusPan")) || !bytes.Contains(ib, []byte("EventSource")) {
		t.Fatalf("index status %d", index.StatusCode)
	}
	if ct := index.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("content-type %s", ct)
	}

	delReq, err := http.NewRequest(http.MethodDelete, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	del, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatal(err)
	}
	defer del.Body.Close()
	if del.StatusCode != 200 {
		t.Fatalf("delete %d", del.StatusCode)
	}
	after := getJSON(t, ts.URL+"/api/jobs")
	if strings.Contains(after, "build 1") {
		t.Fatalf("job remained: %s", after)
	}
}

func TestBodyLimit(t *testing.T) {
	_, ts := newTestServer(t)
	note := strings.Repeat("x", 1<<20)
	body := `{"agent":"a","job_id":"j","state":"running","note":"` + note + `"}`
	resp := postRaw(t, ts.URL, body)
	if resp.StatusCode != 400 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestSSEEmitsJobAndKeepalive(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv := New(st)
	srv.ping = 20 * time.Millisecond
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type %s", ct)
	}
	reader := bufio.NewReader(resp.Body)
	ev, data := readSSE(t, reader)
	if ev != "snapshot" || !strings.Contains(data, `"jobs"`) || !strings.Contains(data, `"abandon_after_seconds":86400`) || !strings.Contains(data, `"abandon_waiting_after_seconds":604800`) {
		t.Fatalf("snapshot: %s %s", ev, data)
	}

	postResp := postRaw(t, ts.URL, `{"agent":"bot","job_id":"j1","state":"running","title":"Hello SSE"}`)
	if postResp.StatusCode != 200 {
		t.Fatalf("post %d %s", postResp.StatusCode, readBody(t, postResp))
	}

	sawJob := false
	sawPing := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && (!sawJob || !sawPing) {
		ev, data = readSSE(t, reader)
		if ev == "job" && strings.Contains(data, `"title":"Hello SSE"`) {
			sawJob = true
		}
		if ev == "comment" {
			sawPing = true
		}
	}
	if !sawJob {
		t.Fatal("no job event")
	}
	if !sawPing {
		t.Fatal("no keepalive")
	}
}

func TestPartialUpdateWithoutState(t *testing.T) {
	_, ts := newTestServer(t)
	resp := postRaw(t, ts.URL, `{"agent":"my bot","job_id":"build 1","note":"too soon"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("new job status %d %s", resp.StatusCode, readBody(t, resp))
	}
	if strings.Contains(getJSON(t, ts.URL+"/api/jobs"), "build 1") {
		t.Fatal("rejected job was stored")
	}

	resp = postRaw(t, ts.URL, `{"agent":"my bot","job_id":"build 1","state":"running","title":"Build v1","progress":10,"link":"http://example.test/a","seq":4}`)
	if resp.StatusCode != 200 {
		t.Fatalf("create %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp = postRaw(t, ts.URL, `{"agent":"my bot","job_id":"build 1","progress":55,"note":"halfway"}`)
	if resp.StatusCode != 200 || !strings.Contains(readBody(t, resp), `"applied":true`) {
		t.Fatalf("partial %d", resp.StatusCode)
	}

	var list struct {
		Jobs []store.JobView `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(getJSON(t, ts.URL+"/api/jobs")), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Jobs) != 1 {
		t.Fatalf("jobs = %d", len(list.Jobs))
	}
	job := list.Jobs[0]
	if job.State != "running" || job.Title != "Build v1" || job.Progress != 55 || job.Note != "halfway" || job.Link != "http://example.test/a" {
		t.Fatalf("merged: %+v", job)
	}
	if job.Seq == nil || *job.Seq != 4 {
		t.Fatalf("seq %v", job.Seq)
	}

	resp = postRaw(t, ts.URL, `[{"agent":"a","job_id":"fresh","state":"queued"},{"agent":"b","job_id":"nope","note":"x"}]`)
	if resp.StatusCode != 400 {
		t.Fatalf("bad batch %d", resp.StatusCode)
	}
	body := getJSON(t, ts.URL+"/api/jobs")
	if strings.Contains(body, `"job_id":"fresh"`) || strings.Contains(body, `"job_id":"nope"`) {
		t.Fatalf("invalid batch applied: %s", body)
	}

	resp = postRaw(t, ts.URL, `[{"agent":"a","job_id":"j","state":"blocked","title":"Wait"},{"agent":"a","job_id":"j","note":"on review"}]`)
	if resp.StatusCode != 200 {
		t.Fatalf("good batch %d %s", resp.StatusCode, readBody(t, resp))
	}
	one := getJSON(t, ts.URL+"/api/jobs/"+url.PathEscape("a")+"/"+url.PathEscape("j"))
	if !strings.Contains(one, `"state":"blocked"`) || !strings.Contains(one, `"note":"on review"`) || !strings.Contains(one, `"title":"Wait"`) {
		t.Fatalf("batch job: %s", one)
	}
}

func TestAgentEventsEndpoint(t *testing.T) {
	_, ts := newTestServer(t)
	for i := 0; i < 25; i++ {
		body := `{"agent":"my bot","job_id":"j","state":"running","seq":` + strconv.Itoa(i+1) + `,"note":"n` + strconv.Itoa(i) + `"}`
		resp := postRaw(t, ts.URL, body)
		if resp.StatusCode != 200 {
			t.Fatalf("post %d: %d %s", i, resp.StatusCode, readBody(t, resp))
		}
	}
	resp := postRaw(t, ts.URL, `{"agent":"other","job_id":"z","state":"queued","title":"Elsewhere"}`)
	if resp.StatusCode != 200 {
		t.Fatal(readBody(t, resp))
	}

	var feed struct {
		Events []store.Event `json:"events"`
	}
	raw := getJSON(t, ts.URL+"/api/events?agent="+url.QueryEscape("my bot"))
	if err := json.Unmarshal([]byte(raw), &feed); err != nil {
		t.Fatal(err)
	}
	if len(feed.Events) != 25 {
		t.Fatalf("agent events = %d", len(feed.Events))
	}
	if feed.Events[0].Note != "n24" || feed.Events[len(feed.Events)-1].Note != "n0" {
		t.Fatalf("order %+v .. %+v", feed.Events[0], feed.Events[len(feed.Events)-1])
	}
	for _, ev := range feed.Events {
		if ev.Agent != "my bot" {
			t.Fatalf("foreign event %+v", ev)
		}
	}

	var job store.JobView
	if err := json.Unmarshal([]byte(getJSON(t, ts.URL+"/api/jobs/"+url.PathEscape("my bot")+"/j")), &job); err != nil {
		t.Fatal(err)
	}
	if len(job.History) >= len(feed.Events) {
		t.Fatalf("history %d was not shorter than the agent feed %d", len(job.History), len(feed.Events))
	}

	var other struct {
		Events []store.Event `json:"events"`
	}
	if err := json.Unmarshal([]byte(getJSON(t, ts.URL+"/api/events?agent=other")), &other); err != nil {
		t.Fatal(err)
	}
	if len(other.Events) != 1 || other.Events[0].JobID != "z" {
		t.Fatalf("other = %+v", other.Events)
	}

	var nobody struct {
		Events []store.Event `json:"events"`
	}
	if err := json.Unmarshal([]byte(getJSON(t, ts.URL+"/api/events?agent="+url.QueryEscape("no such"))), &nobody); err != nil {
		t.Fatal(err)
	}
	if len(nobody.Events) != 0 {
		t.Fatalf("unknown agent = %+v", nobody.Events)
	}

	limited := getJSON(t, ts.URL+"/api/events?agent="+url.QueryEscape("my bot")+"&limit=2")
	var lim struct {
		Events []store.Event `json:"events"`
	}
	if err := json.Unmarshal([]byte(limited), &lim); err != nil {
		t.Fatal(err)
	}
	if len(lim.Events) != 2 || lim.Events[0].Note != "n24" {
		t.Fatalf("limited = %+v", lim.Events)
	}

	bad, err := http.Get(ts.URL + "/api/events?agent=my%20bot&limit=no")
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Body.Close()
	if bad.StatusCode != 400 {
		t.Fatalf("bad limit %d", bad.StatusCode)
	}
}

func TestClearAllRequiresConfirmAndBacksUp(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ts := httptest.NewServer(New(st).Handler())
	t.Cleanup(ts.Close)

	resp := postRaw(t, ts.URL, `{"agent":"bot","job_id":"build-1","state":"running","title":"Keep me","note":"first"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("post %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp = postRaw(t, ts.URL, `{"agent":"bot","job_id":"build-1","progress":20,"note":"second"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("update %d %s", resp.StatusCode, readBody(t, resp))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	streamReq, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	streamResp, err := http.DefaultClient.Do(streamReq)
	if err != nil {
		t.Fatal(err)
	}
	defer streamResp.Body.Close()
	reader := bufio.NewReader(streamResp.Body)
	ev, data := readSSE(t, reader)
	if ev != "snapshot" || !strings.Contains(data, "Keep me") {
		t.Fatalf("initial snapshot %s %s", ev, data)
	}

	refused := []struct {
		url  string
		body string
	}{
		{ts.URL + "/api/jobs", ""},
		{ts.URL + "/api/jobs?confirm=yes", ""},
		{ts.URL + "/api/jobs", `{"confirm":"no"}`},
		{ts.URL + "/api/jobs", `{`},
	}
	for _, c := range refused {
		req, err := http.NewRequest(http.MethodDelete, c.url, strings.NewReader(c.body))
		if err != nil {
			t.Fatal(err)
		}
		got, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(got.Body)
		got.Body.Close()
		if got.StatusCode != 400 {
			t.Fatalf("DELETE %s %q: %d %s", c.url, c.body, got.StatusCode, b)
		}
	}
	if !strings.Contains(getJSON(t, ts.URL+"/api/jobs"), "Keep me") {
		t.Fatal("refused clear changed the board")
	}
	backups, err := filepath.Glob(filepath.Join(dir, "jobs-backup-*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 0 {
		t.Fatalf("backup written without confirm: %v", backups)
	}

	req, err := http.NewRequest(http.MethodDelete, ts.URL+"/api/jobs?confirm=all", nil)
	if err != nil {
		t.Fatal(err)
	}
	cleared, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, cleared)
	if cleared.StatusCode != 200 {
		t.Fatalf("clear %d %s", cleared.StatusCode, body)
	}
	var out struct {
		OK      bool   `json:"ok"`
		Deleted int    `json:"deleted"`
		Backup  string `json:"backup"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.Deleted != 1 || out.Backup == "" {
		t.Fatalf("clear response %s", body)
	}
	if !strings.HasPrefix(out.Backup, dir) {
		t.Fatalf("backup %s not in %s", out.Backup, dir)
	}
	saved, err := os.ReadFile(out.Backup)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "Keep me") || !strings.Contains(string(saved), "first") {
		t.Fatalf("backup %s", saved)
	}
	jobs := getJSON(t, ts.URL+"/api/jobs")
	if strings.Contains(jobs, "Keep me") || strings.Contains(jobs, "build-1") {
		t.Fatalf("jobs after clear %s", jobs)
	}
	var list struct {
		Jobs []store.JobView `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(jobs), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Jobs) != 0 {
		t.Fatalf("jobs %d", len(list.Jobs))
	}
	events := getJSON(t, ts.URL+"/api/events")
	var feed struct {
		Events []store.Event `json:"events"`
	}
	if err := json.Unmarshal([]byte(events), &feed); err != nil {
		t.Fatal(err)
	}
	if len(feed.Events) != 0 {
		t.Fatalf("events %+v", feed.Events)
	}

	sawEmpty := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !sawEmpty {
		ev, data = readSSE(t, reader)
		if ev == "snapshot" && strings.Contains(data, `"jobs":[]`) {
			sawEmpty = true
		}
	}
	if !sawEmpty {
		t.Fatalf("no empty snapshot, last %s %s", ev, data)
	}

	req, err = http.NewRequest(http.MethodDelete, ts.URL+"/api/jobs", strings.NewReader(`{"confirm":"all"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	again, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	againBody := readBody(t, again)
	if again.StatusCode != 200 || !strings.Contains(againBody, `"deleted":0`) {
		t.Fatalf("second clear %d %s", again.StatusCode, againBody)
	}
	var out2 struct {
		Backup string `json:"backup"`
	}
	if err := json.Unmarshal([]byte(againBody), &out2); err != nil {
		t.Fatal(err)
	}
	if out2.Backup == "" || out2.Backup == out.Backup {
		t.Fatalf("second backup %q first %q", out2.Backup, out.Backup)
	}
	if _, err := os.Stat(out2.Backup); err != nil {
		t.Fatal(err)
	}

	ts.Close()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, jobs := reopened.List(); len(jobs) != 0 {
		t.Fatalf("replayed %d jobs", len(jobs))
	}
	if _, evs := reopened.Events(20); len(evs) != 0 {
		t.Fatalf("replayed %d events", len(evs))
	}
}

func TestIndexSelfContained(t *testing.T) {
	html := string(indexHTML)
	for _, needle := range []string{"StatusPan", "EventSource", "/api/stream", "STALLED", "statuspan-theme", "/api/events", "#/agent/", "#/job/", "prefers-color-scheme", "Needs attention", "__statuspan", "Clear all", "Offline", "statuspan-expand-queued"} {
		if !strings.Contains(html, needle) {
			t.Fatalf("index missing %s", needle)
		}
	}
	if strings.Contains(html, "http://") || strings.Contains(html, "https://") {
		t.Fatal("index references an external URL")
	}
}

func TestValidateAddr(t *testing.T) {
	ok := []string{"127.0.0.1:4477", "localhost:4477", "[::1]:4477", "127.0.0.1:0"}
	for _, addr := range ok {
		if err := ValidateAddr(addr); err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
	}
	bad := []string{"0.0.0.0:4477", ":4477", "192.168.1.5:4477", "example.com:4477", "127.0.0.1"}
	for _, addr := range bad {
		if err := ValidateAddr(addr); err == nil {
			t.Fatalf("accepted %s", addr)
		}
	}
}

func getJSON(t *testing.T, rawURL string) string {
	t.Helper()
	resp, err := http.Get(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s: %d %s", rawURL, resp.StatusCode, b)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type %s", ct)
	}
	return string(b)
}

func readSSE(t *testing.T, r *bufio.Reader) (string, string) {
	t.Helper()
	var event, data string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("sse read: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if event != "" || data != "" {
				return event, data
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			return "comment", strings.TrimSpace(strings.TrimPrefix(line, ":"))
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if strings.HasPrefix(line, "data:") {
			data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
}

func TestHealthIncludesRelayStatusWithoutToken(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	api := New(st)
	when := time.Date(2026, 4, 2, 15, 4, 5, 0, time.UTC)
	const secret = "read-token-do-not-leak"
	api.SetRelay(func() relay.Status {
		return relay.Status{Configured: true, LastOK: &when, Cursor: 12, LastError: "offline"}
	})
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status %d %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), secret) {
		t.Fatalf("health leaked token: %s", body)
	}
	var health struct {
		OK    bool         `json:"ok"`
		Relay relay.Status `json:"relay"`
	}
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatal(err)
	}
	if !health.OK || !health.Relay.Configured || health.Relay.Cursor != 12 || health.Relay.LastError != "offline" || health.Relay.LastOK == nil {
		t.Fatalf("%s", body)
	}
	if !health.Relay.LastOK.Equal(when) {
		t.Fatalf("last_ok %s", health.Relay.LastOK)
	}
}
