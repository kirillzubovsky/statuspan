package server

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed static/smart-title-worker.js
var smartTitleWorkerJS []byte

const (
	transformersVersion = "4.3.2"
	// onnxWebVersion is the onnxruntime-web pin inside @huggingface/transformers@4.3.2.
	onnxWebVersion = "1.31.0-dev.20260914-8d85527a0"
	smartModelID   = "onnx-community/Qwen2.5-0.5B-Instruct"
	// smartTitleMaxBytes is a ceiling above the q4f16 weights (~483 MB).
	smartTitleMaxBytes int64 = 700 << 20
)

var (
	errSmartOff  = errors.New("smart titles unavailable")
	errSmartPath = errors.New("invalid path")
	errSmartHost = errors.New("redirect host not allowed")
	errSmartSize = errors.New("file too large")
	errSmartGet  = errors.New("download failed")
	// errBodyStarted means bytes were already written, so the status cannot change.
	errBodyStarted = errors.New("body started")
)

func (s *Server) handleSmartWorker(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(smartTitleWorkerJS)
}

func (s *Server) handleSmartLib(w http.ResponseWriter, r *http.Request) {
	s.serveSmart(w, r, "lib", r.PathValue("path"))
}

func (s *Server) handleSmartModel(w http.ResponseWriter, r *http.Request) {
	s.serveSmart(w, r, "model", r.PathValue("path"))
}

func (s *Server) serveSmart(w http.ResponseWriter, r *http.Request, kind, rel string) {
	if s.smartCache == "" {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	upstream, err := smartUpstream(kind, rel)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid path")
		return
	}
	dest, err := smartCacheFile(s.smartCache, kind, rel)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid path")
		return
	}
	unlock := s.lockSmart(dest)
	defer unlock()
	if fileReady(dest) {
		serveCached(w, r, dest)
		return
	}
	err = s.pullSmart(w, r, upstream, dest, kind == "lib" && rel == "transformers.web.js")
	if err != nil && !errors.Is(err, errBodyStarted) {
		writeSmartErr(w, err)
	}
}

func (s *Server) lockSmart(key string) func() {
	s.smartMu.Lock()
	if s.smartKeys == nil {
		s.smartKeys = make(map[string]*sync.Mutex)
	}
	m := s.smartKeys[key]
	if m == nil {
		m = &sync.Mutex{}
		s.smartKeys[key] = m
	}
	s.smartMu.Unlock()
	m.Lock()
	return m.Unlock
}

func (s *Server) smartHTTP() *http.Client {
	s.smartMu.Lock()
	defer s.smartMu.Unlock()
	if s.smartClient == nil {
		s.smartClient = newSmartClient()
	}
	return s.smartClient
}

func newSmartClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 8 {
				return errSmartHost
			}
			return allowSmartHop(via[0].URL, req.URL)
		},
		Transport: &http.Transport{
			Proxy:                 func(*http.Request) (*url.URL, error) { return nil, nil },
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 45 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
	}
}

func (s *Server) pullSmart(w http.ResponseWriter, r *http.Request, upstream *url.URL, dest string, rewrite bool) error {
	if smallRange(r.Header.Get("Range")) && !rewrite {
		return s.proxySmallRange(w, r, upstream)
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstream.String(), nil)
	if err != nil {
		return errSmartGet
	}
	req.Header.Set("User-Agent", "statuspan")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := s.smartHTTP().Do(req)
	if err != nil {
		return errSmartGet
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errSmartGet
	}
	if resp.ContentLength > smartTitleMaxBytes {
		return errSmartSize
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if rewrite {
		return pullRewrite(resp.Body, dest, w)
	}
	return pullStream(resp, dest, w)
}

func pullRewrite(body io.Reader, dest string, w http.ResponseWriter) error {
	raw, err := io.ReadAll(io.LimitReader(body, smartTitleMaxBytes+1))
	if err != nil {
		return errSmartGet
	}
	if int64(len(raw)) > smartTitleMaxBytes {
		return errSmartSize
	}
	out, err := rewriteTransformersJS(raw)
	if err != nil {
		return errSmartGet
	}
	if err := writeCache(dest, out); err != nil {
		return err
	}
	writeJS(w, out)
	return nil
}

func pullStream(resp *http.Response, dest string, w http.ResponseWriter) error {
	part := dest + ".part"
	f, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		f.Close()
		if !keep {
			os.Remove(part)
		}
	}()
	header := w.Header()
	header.Set("Content-Type", contentType(dest))
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Cache-Control", "private, max-age=86400")
	if resp.ContentLength >= 0 {
		header.Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	buf := make([]byte, 32*1024)
	var n int64
	wrote := false
	for {
		nr, er := resp.Body.Read(buf)
		if nr > 0 {
			n += int64(nr)
			if n > smartTitleMaxBytes {
				if wrote {
					return errBodyStarted
				}
				return errSmartSize
			}
			if _, err := f.Write(buf[:nr]); err != nil {
				return err
			}
			if _, err := w.Write(buf[:nr]); err != nil {
				return errBodyStarted
			}
			wrote = true
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
		if er == io.EOF {
			break
		}
		if er != nil {
			if wrote {
				return errBodyStarted
			}
			return errSmartGet
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(part, dest); err != nil {
		return err
	}
	keep = true
	return nil
}

func (s *Server) proxySmallRange(w http.ResponseWriter, r *http.Request, upstream *url.URL) error {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstream.String(), nil)
	if err != nil {
		return errSmartGet
	}
	req.Header.Set("User-Agent", "statuspan")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Range", r.Header.Get("Range"))
	resp, err := s.smartHTTP().Do(req)
	if err != nil {
		return errSmartGet
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return errSmartGet
	}
	if total, ok := rangeTotal(resp); ok && total > smartTitleMaxBytes {
		return errSmartSize
	}
	if resp.ContentLength > smartTitleMaxBytes {
		return errSmartSize
	}
	limit := int64(1 << 20)
	if resp.ContentLength > limit || (resp.StatusCode == http.StatusOK && resp.ContentLength < 0) {
		return errSmartGet
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return errSmartGet
	}
	if int64(len(body)) > limit {
		return errSmartGet
	}
	header := w.Header()
	header.Set("X-Content-Type-Options", "nosniff")
	copyHeader(header, resp.Header, "Content-Type", "Content-Range", "Accept-Ranges")
	if header.Get("Content-Type") == "" {
		header.Set("Content-Type", "application/octet-stream")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
	return nil
}

func copyHeader(dst, src http.Header, keys ...string) {
	for _, k := range keys {
		if v := src.Get(k); v != "" {
			dst.Set(k, v)
		}
	}
}

func rangeTotal(resp *http.Response) (int64, bool) {
	cr := resp.Header.Get("Content-Range")
	i := strings.LastIndex(cr, "/")
	if i < 0 || i == len(cr)-1 || cr[i+1:] == "*" {
		return 0, false
	}
	n, err := strconv.ParseInt(cr[i+1:], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func smallRange(h string) bool {
	h = strings.TrimSpace(h)
	if !strings.HasPrefix(h, "bytes=") || strings.Contains(h, ",") {
		return false
	}
	span := strings.TrimPrefix(h, "bytes=")
	startS, endS, ok := strings.Cut(span, "-")
	if !ok || startS == "" || endS == "" {
		return false
	}
	start, err1 := strconv.ParseInt(startS, 10, 64)
	end, err2 := strconv.ParseInt(endS, 10, 64)
	if err1 != nil || err2 != nil || start < 0 || end < start {
		return false
	}
	return end-start+1 <= 1<<20
}

func writeCache(dest string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	part := dest + ".part"
	if err := os.WriteFile(part, body, 0o644); err != nil {
		return err
	}
	return os.Rename(part, dest)
}

func writeJS(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = w.Write(body)
}

func serveCached(w http.ResponseWriter, r *http.Request, path string) {
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "download failed")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeErr(w, http.StatusBadGateway, "download failed")
		return
	}
	w.Header().Set("Content-Type", contentType(path))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, st.Name(), st.ModTime(), f)
}

func fileReady(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular() && st.Size() > 0
}

func contentType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".wasm":
		return "application/wasm"
	case ".json":
		return "application/json"
	default:
		return "application/octet-stream"
	}
}

func writeSmartErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errSmartSize):
		writeErr(w, http.StatusRequestEntityTooLarge, "file too large")
	case errors.Is(err, errSmartPath):
		writeErr(w, http.StatusBadRequest, "invalid path")
	case errors.Is(err, errSmartOff):
		writeErr(w, http.StatusNotFound, "not found")
	default:
		writeErr(w, http.StatusBadGateway, "download failed")
	}
}

func smartCacheFile(root, kind, rel string) (string, error) {
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" || strings.Contains(rel, "..") || strings.Contains(rel, "\\") || strings.Contains(rel, "\x00") {
		return "", errSmartPath
	}
	dest := filepath.Join(root, kind, filepath.FromSlash(rel))
	base := filepath.Join(root, kind)
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return "", err
	}
	sep := string(filepath.Separator)
	if absDest != absBase && !strings.HasPrefix(absDest, absBase+sep) {
		return "", errSmartPath
	}
	return dest, nil
}

func smartUpstream(kind, rel string) (*url.URL, error) {
	switch kind {
	case "lib":
		return smartLibUpstream(rel)
	case "model":
		return smartModelUpstream(rel)
	default:
		return nil, errSmartPath
	}
}

func smartLibUpstream(rel string) (*url.URL, error) {
	rel = strings.TrimPrefix(rel, "/")
	if !safeRel(rel) {
		return nil, errSmartPath
	}
	var raw string
	switch rel {
	case "transformers.web.js":
		raw = "https://cdn.jsdelivr.net/npm/@huggingface/transformers@" + transformersVersion + "/dist/transformers.web.js"
	case "ort.webgpu.bundle.min.mjs":
		raw = "https://cdn.jsdelivr.net/npm/onnxruntime-web@" + onnxWebVersion + "/dist/ort.webgpu.bundle.min.mjs"
	case "ort/ort-wasm-simd-threaded.asyncify.mjs", "ort-wasm-simd-threaded.asyncify.mjs":
		raw = onnxDist("ort-wasm-simd-threaded.asyncify.mjs")
	case "ort/ort-wasm-simd-threaded.asyncify.wasm", "ort-wasm-simd-threaded.asyncify.wasm":
		raw = onnxDist("ort-wasm-simd-threaded.asyncify.wasm")
	case "ort/ort-wasm-simd-threaded.mjs", "ort-wasm-simd-threaded.mjs":
		raw = onnxDist("ort-wasm-simd-threaded.mjs")
	case "ort/ort-wasm-simd-threaded.wasm", "ort-wasm-simd-threaded.wasm":
		raw = onnxDist("ort-wasm-simd-threaded.wasm")
	default:
		return nil, errSmartPath
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.RawQuery != "" || u.Host != "cdn.jsdelivr.net" {
		return nil, errSmartPath
	}
	return u, nil
}

func onnxDist(name string) string {
	return "https://cdn.jsdelivr.net/npm/onnxruntime-web@" + onnxWebVersion + "/dist/" + name
}

func smartModelUpstream(rel string) (*url.URL, error) {
	rel = strings.TrimPrefix(rel, "/")
	if !safeRel(rel) {
		return nil, errSmartPath
	}
	const prefix = smartModelID + "/resolve/main/"
	if !strings.HasPrefix(rel, prefix) {
		return nil, errSmartPath
	}
	rest := strings.TrimPrefix(rel, prefix)
	parts := strings.Split(rest, "/")
	switch len(parts) {
	case 1:
		if !safeSeg(parts[0]) {
			return nil, errSmartPath
		}
	case 2:
		if parts[0] != "onnx" || !safeSeg(parts[1]) {
			return nil, errSmartPath
		}
	default:
		return nil, errSmartPath
	}
	u, err := url.Parse("https://huggingface.co/" + rel)
	if err != nil {
		return nil, err
	}
	if u.Host != "huggingface.co" || u.RawQuery != "" {
		return nil, errSmartPath
	}
	return u, nil
}

func safeRel(rel string) bool {
	if rel == "" || strings.Contains(rel, "\\") || strings.Contains(rel, "\x00") || strings.Contains(rel, "..") {
		return false
	}
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func safeSeg(s string) bool {
	if s == "" || s == "." || s == ".." || strings.Contains(s, "..") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// allowSmartHop permits a redirect from the pinned upstream origin only.
// Query strings are not logged. Callers must not print next.String().
func allowSmartHop(origin, next *url.URL) error {
	if origin == nil || next == nil || next.Scheme != "https" || next.User != nil {
		return errSmartHost
	}
	host := strings.ToLower(next.Hostname())
	if host == "" || net.ParseIP(host) != nil {
		return errSmartHost
	}
	switch strings.ToLower(origin.Hostname()) {
	case "cdn.jsdelivr.net":
		if !jsDelivrHost(host) || next.Path != origin.Path {
			return errSmartHost
		}
		return nil
	case "huggingface.co":
		return allowHFHop(origin, next)
	default:
		return errSmartHost
	}
}

func allowHFHop(origin, next *url.URL) error {
	host := strings.ToLower(next.Hostname())
	if host == "huggingface.co" {
		if next.Path == origin.Path {
			return nil
		}
		const cache = "/api/resolve-cache/models/" + smartModelID + "/"
		if strings.HasPrefix(next.Path, cache) {
			return nil
		}
		return errSmartHost
	}
	if hfDownloadHost(host) {
		return nil
	}
	return errSmartHost
}

func jsDelivrHost(host string) bool {
	switch host {
	case "cdn.jsdelivr.net", "fastly.jsdelivr.net", "gcore.jsdelivr.net":
		return true
	default:
		return false
	}
}

func hfDownloadHost(host string) bool {
	if host == "huggingface.co" {
		return true
	}
	if strings.HasSuffix(host, ".huggingface.co") && host != ".huggingface.co" {
		return true
	}
	return strings.HasSuffix(host, ".hf.co") && strings.Count(host, ".") >= 2
}

func rewriteTransformersJS(in []byte) ([]byte, error) {
	const oldImport = `import * as ONNX_WEB from "onnxruntime-web/webgpu";`
	const newImport = `import * as ONNX_WEB from "/api/smart-title/lib/ort.webgpu.bundle.min.mjs";`
	const oldPrefix = "https://cdn.jsdelivr.net/npm/onnxruntime-web@${ONNX_ENV.versions.web}/dist/"
	const newPrefix = "/api/smart-title/lib/ort/"
	s := string(in)
	if !strings.Contains(s, oldImport) || !strings.Contains(s, oldPrefix) {
		return nil, fmt.Errorf("transformers.js layout changed")
	}
	s = strings.ReplaceAll(s, oldImport, newImport)
	s = strings.ReplaceAll(s, oldPrefix, newPrefix)
	if strings.Contains(s, "cdn.jsdelivr.net/npm/onnxruntime") || strings.Contains(s, `from "onnxruntime-web/webgpu"`) {
		return nil, fmt.Errorf("transformers.js rewrite incomplete")
	}
	return []byte(s), nil
}
