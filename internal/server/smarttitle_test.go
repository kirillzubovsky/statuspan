package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kirillzubovsky/statuspan/internal/store"
)

func TestTitleOverlayHTTP(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ts := httptest.NewServer(New(st).Handler())
	t.Cleanup(ts.Close)

	raw := "Please rewrite this very long and messy job title that keeps going"
	create, err := json.Marshal(map[string]any{
		"agent": "my bot", "job_id": "build 1", "state": "running",
		"title": raw, "seq": 1, "progress": 12, "note": "n",
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := postRaw(t, ts.URL, string(create))
	if resp.StatusCode != 200 {
		t.Fatalf("create %d %s", resp.StatusCode, readBody(t, resp))
	}
	jobURL := ts.URL + "/api/jobs/" + url.PathEscape("my bot") + "/" + url.PathEscape("build 1")
	var before store.JobView
	if err := json.Unmarshal([]byte(getJSON(t, jobURL)), &before); err != nil {
		t.Fatal(err)
	}
	if before.Title != raw || before.RawTitle != "" || before.Seq == nil || *before.Seq != 1 {
		t.Fatalf("before %+v", before)
	}

	titleURL := jobURL + "/title"
	missing := ts.URL + "/api/jobs/" + url.PathEscape("no such") + "/" + url.PathEscape("build 1") + "/title"
	if code, body := postTitle(t, missing, `{"title":"Short overlay title"}`); code != 404 || !strings.Contains(body, "not found") {
		t.Fatalf("missing %d %s", code, body)
	}
	bad := []string{"{", "", `{"title":""}`, `{"title":"   "}`, `{"title":null}`, `{}`}
	for _, payload := range bad {
		code, body := postTitle(t, titleURL, payload)
		if code != 400 {
			t.Fatalf("payload %q: %d %s", payload, code, body)
		}
	}
	var still store.JobView
	if err := json.Unmarshal([]byte(getJSON(t, jobURL)), &still); err != nil {
		t.Fatal(err)
	}
	if still.Title != raw || !still.UpdatedAt.Equal(before.UpdatedAt) || len(still.History) != len(before.History) {
		t.Fatalf("rejected title changed the job: %+v", still)
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
	if ev != "snapshot" || !strings.Contains(data, raw) {
		t.Fatalf("snapshot %s %s", ev, data)
	}

	short := "Rewrite the messy title"
	payload, err := json.Marshal(map[string]string{"title": short})
	if err != nil {
		t.Fatal(err)
	}
	if code, body := postTitle(t, titleURL, string(payload)); code != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("overlay %d %s", code, body)
	}
	ev, data = readSSE(t, reader)
	if ev != "job" {
		t.Fatalf("sse %s %s", ev, data)
	}
	var pushed store.JobView
	if err := json.Unmarshal([]byte(data), &pushed); err != nil {
		t.Fatal(err)
	}
	if pushed.Title != short || pushed.RawTitle != raw || !pushed.UpdatedAt.Equal(before.UpdatedAt) || pushed.Seq == nil || *pushed.Seq != 1 {
		t.Fatalf("sse job %+v", pushed)
	}

	var after store.JobView
	if err := json.Unmarshal([]byte(getJSON(t, jobURL)), &after); err != nil {
		t.Fatal(err)
	}
	if after.Title != short || after.RawTitle != raw || !after.UpdatedAt.Equal(before.UpdatedAt) || !after.StartedAt.Equal(before.StartedAt) {
		t.Fatalf("get %+v", after)
	}
	if after.Seq == nil || *after.Seq != *before.Seq || after.Progress != before.Progress || after.Note != before.Note || after.State != before.State || len(after.History) != len(before.History) {
		t.Fatalf("overlay changed job fields: %+v", after)
	}
	list := getJSON(t, ts.URL+"/api/jobs")
	if !strings.Contains(list, short) || !strings.Contains(list, `"raw_title":"`+raw+`"`) {
		t.Fatalf("list missing overlay: %s", list)
	}
	logged, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logged), short) {
		t.Fatal("overlay title written to the log")
	}
}

func postTitle(t *testing.T, rawURL, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(rawURL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func TestSmartUpstreamAllowlist(t *testing.T) {
	okLib := []string{
		"transformers.web.js",
		"ort.webgpu.bundle.min.mjs",
		"ort/ort-wasm-simd-threaded.asyncify.wasm",
		"ort-wasm-simd-threaded.asyncify.mjs",
		"ort/ort-wasm-simd-threaded.wasm",
		"ort-wasm-simd-threaded.mjs",
		"ort/ort-wasm-simd-threaded.jsep.mjs",
		"ort-wasm-simd-threaded.jsep.mjs",
		"ort/ort-wasm-simd-threaded.jsep.wasm",
		"ort-wasm-simd-threaded.jsep.wasm",
	}
	for _, rel := range okLib {
		u, err := smartLibUpstream(rel)
		if err != nil || u.Scheme != "https" || u.Host != "cdn.jsdelivr.net" || u.RawQuery != "" {
			t.Fatalf("%s: %v %v", rel, u, err)
		}
		if _, err := smartUpstream("lib", rel); err != nil {
			t.Fatal(rel, err)
		}
		if strings.Contains(rel, "ort-wasm-") {
			name := rel
			if i := strings.LastIndex(rel, "/"); i >= 0 {
				name = rel[i+1:]
			}
			if got, want := u.String(), onnxDist(name); got != want {
				t.Fatalf("%s: got %s want %s", rel, got, want)
			}
		}
	}
	for _, rel := range []string{
		smartModelID + "/resolve/main/config.json",
		smartModelID + "/resolve/main/onnx/model_uint8.onnx",
	} {
		u, err := smartModelUpstream(rel)
		if err != nil || u.Scheme != "https" || u.Host != "huggingface.co" || u.RawQuery != "" {
			t.Fatalf("%s: %v %v", rel, u, err)
		}
		if _, err := smartUpstream("model", rel); err != nil {
			t.Fatal(rel, err)
		}
	}
	badModel := []string{
		"other/model/resolve/main/config.json",
		smartModelID + "/resolve/other/config.json",
		"../" + smartModelID + "/resolve/main/config.json",
		smartModelID + "/resolve/main/../../config.json",
		smartModelID + "/resolve/main/onnx/../config.json",
		smartModelID + "/resolve/main/a/b/c.json",
	}
	for _, rel := range badModel {
		if _, err := smartModelUpstream(rel); err == nil {
			t.Fatalf("allowed model %s", rel)
		}
		if _, err := smartUpstream("model", rel); err == nil {
			t.Fatalf("allowed via smartUpstream %s", rel)
		}
	}
	for _, rel := range []string{"../transformers.web.js", "not-a-file.js", "ort/other.wasm", `..\transformers.web.js`} {
		if _, err := smartLibUpstream(rel); err == nil {
			t.Fatalf("allowed lib %s", rel)
		}
	}
	if !safeRel("transformers.web.js") || !safeRel("a/b") {
		t.Fatal("safeRel rejected a normal path")
	}
	if safeRel("") || safeRel("..") || safeRel("a/../b") || safeRel("a/./b") || safeRel(`a\b`) || safeRel("a//b") {
		t.Fatal("safeRel accepted a bad path")
	}
}

type denyTrip struct {
	t *testing.T
	n int
}

func (d *denyTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	d.n++
	d.t.Errorf("dialed host %s", r.URL.Host)
	return nil, errors.New("dialed")
}

func TestSmartProxyRejectsBeforeDial(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	deny := &denyTrip{t: t}
	api := New(st)
	api.smartClient = &http.Client{Transport: deny}
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/smart-title/lib/transformers.web.js")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "not found") {
		t.Fatalf("cache unset %d %s", resp.StatusCode, body)
	}
	if deny.n != 0 {
		t.Fatalf("cache unset dialed %d", deny.n)
	}

	api.SetSmartTitleCache(t.TempDir())
	paths := []string{
		"/api/smart-title/model/other/Model/resolve/main/config.json",
		"/api/smart-title/lib/not-a-file.js",
		"/api/smart-title/model/" + smartModelID + "/resolve/main/../../secret",
	}
	for _, path := range paths {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), "invalid path") {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, b)
		}
	}
	if deny.n != 0 {
		t.Fatalf("bad path dialed %d", deny.n)
	}
}

func TestRewriteTransformersJSTensorImport(t *testing.T) {
	fixture := "" +
		"import * as ONNX_WEB from \"onnxruntime-web/webgpu\";\n" +
		"import { Tensor } from \"onnxruntime-common\";\n" +
		"const prefix = \"https://cdn.jsdelivr.net/npm/onnxruntime-web@${ONNX_ENV.versions.web}/dist/\";\n" +
		"// comment only: \"onnxruntime-common\"\n" +
		"/* block comment only: 'onnxruntime-common' */\n"
	out, err := rewriteTransformersJS([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	const wantTensor = `import { Tensor } from "/api/smart-title/lib/ort.webgpu.bundle.min.mjs";`
	const wantWebGPU = `import * as ONNX_WEB from "/api/smart-title/lib/ort.webgpu.bundle.min.mjs";`
	if !strings.Contains(s, wantTensor) || !strings.Contains(s, wantWebGPU) {
		t.Fatalf("imports not rewritten: %s", s)
	}
	if !strings.Contains(s, "/api/smart-title/lib/ort/") {
		t.Fatalf("wasm prefix not rewritten: %s", s)
	}
	if strings.Contains(s, `from "onnxruntime-web/webgpu"`) || strings.Contains(s, "cdn.jsdelivr.net/npm/onnxruntime") {
		t.Fatalf("original specifier remains: %s", s)
	}
	if bareONNXRuntimeCommon(s) {
		t.Fatalf("bare onnxruntime-common remains: %s", s)
	}
	if !strings.Contains(s, `comment only: "onnxruntime-common"`) {
		t.Fatal("comment mention was rewritten")
	}

	leftover := fixture + "import { env } from \"onnxruntime-common\";\n"
	if _, err := rewriteTransformersJS([]byte(leftover)); err == nil {
		t.Fatal("leftover bare specifier was accepted")
	}
}

func TestSmartWorkerSelfContained(t *testing.T) {
	js := string(smartTitleWorkerJS)
	if strings.Contains(js, "http://") || strings.Contains(js, "https://") {
		t.Fatal("worker references an external URL")
	}
	if strings.Contains(js, "jsdelivr") || strings.Contains(js, "huggingface.co") {
		t.Fatal("worker names a third-party host")
	}
	if !strings.Contains(js, "/api/smart-title/lib/transformers.web.js") || !strings.Contains(js, "/api/smart-title/model/") {
		t.Fatal("worker does not use the local proxy")
	}
	if !strings.Contains(js, "requestAdapter") {
		t.Fatal("worker does not probe requestAdapter")
	}
	if !strings.Contains(js, "fallback-wasm") {
		t.Fatal("worker does not signal fallback-wasm")
	}
	if strings.Contains(js, `"q4"`) {
		t.Fatal(`worker uses dtype "q4"`)
	}
	if !strings.Contains(js, "q4f16") {
		t.Fatal("worker dropped webgpu q4f16")
	}
}
