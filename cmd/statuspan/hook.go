package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/kirillzubovsky/statuspan/internal/server"
	"github.com/kirillzubovsky/statuspan/internal/store"
)

const (
	hookOverallTimeout = 2 * time.Second
	hookHTTPTimeout    = 1500 * time.Millisecond
	hookMaxBody        = 1 << 20
	hookTTL            = 900
	hookTitleMax       = 80
	hookNoteMax        = 100
	defaultHookAgent   = "Claude Code"
	// SessionStart can be delivered after UserPromptSubmit. A seq 2s in the
	// past lets any later event of the session win.
	sessionStartSeqBackdate = 2000
)

// claudeFailMarkerDir holds one empty file per session that ended a turn in
// StopFailure. Each hook event is a new process, so SessionEnd cannot see
// that failure unless it is written down. Tests replace this directory.
var claudeFailMarkerDir = filepath.Join(os.TempDir(), "statuspan-claude")

// hookCmd is `statuspan hook`. Stdout stays empty: Claude feeds hook stdout
// back into the session as context.
func hookCmd(args []string) error {
	if len(args) == 0 || isHelpArg(args[0]) {
		usage()
		return nil
	}
	if args[0] != "claude" {
		return fmt.Errorf("unknown hook %q (try statuspan hook claude)", args[0])
	}
	return hookClaude(args[1:])
}

func hookClaude(args []string) error {
	fs := newFlagSet("hook claude")
	agent := fs.String("agent", envOr("STATUSPAN_AGENT", defaultHookAgent), "agent name (default STATUSPAN_AGENT or \"Claude Code\")")
	jobURL := fs.String("url", "", "full jobs endpoint (overrides --addr; env STATUSPAN_URL)")
	addr := fs.String("addr", envOr("STATUSPAN_ADDR", server.DefaultAddr), "server address")
	relayURL := fs.String("relay", "", "post to this relay instead of the local daemon")
	workspace := fs.String("workspace", "", "relay workspace")
	token := fs.String("token", "", "relay write token")
	tokenFile := fs.String("token-file", "", "file containing the relay write token")
	verbose := fs.Bool("verbose", false, "print warnings on stderr")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Args()[0])
	}
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	warn := func(err error) {
		if err == nil || !*verbose {
			return
		}
		msg := err.Error()
		if seen["token"] {
			msg = strings.ReplaceAll(msg, *token, "[redacted]")
		}
		if env := strings.TrimSpace(os.Getenv("STATUSPAN_TOKEN")); env != "" {
			msg = strings.ReplaceAll(msg, env, "[redacted]")
		}
		fmt.Fprintf(os.Stderr, "statuspan: %s\n", msg)
	}
	if err := postClaudeHook(seen, strings.TrimSpace(*agent), *jobURL, *addr, *relayURL, *workspace, *token, *tokenFile); err != nil {
		warn(err)
	}
	return nil
}

func postClaudeHook(seen map[string]bool, agent, jobURL, addr, relayURL, workspace, token, tokenFile string) error {
	started := time.Now()
	body, err := readHookStdin(hookOverallTimeout)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	in, err := parseClaudeHook(body)
	if err != nil {
		return err
	}
	if agent == "" {
		agent = defaultHookAgent
	}
	update, ok := claudeUpdate(in, agent)
	if !ok {
		return nil
	}
	if err := store.Validate(update); err != nil {
		return err
	}
	endpointURL, tok, err := resolveRunEndpoint(seen, jobURL, addr, relayURL, workspace, token, tokenFile)
	if err != nil {
		return err
	}
	remain := hookOverallTimeout - time.Since(started)
	httpTimeout := hookHTTPTimeout
	if remain < httpTimeout {
		httpTimeout = remain
	}
	if httpTimeout <= 0 {
		return fmt.Errorf("hook deadline exceeded")
	}
	p := &poster{
		url:     endpointURL,
		token:   tok,
		verbose: false,
		client:  &http.Client{Timeout: httpTimeout},
	}
	return p.send(update)
}

func readHookStdin(budget time.Duration) ([]byte, error) {
	if budget <= 0 {
		return nil, fmt.Errorf("hook deadline exceeded")
	}
	if err := os.Stdin.SetReadDeadline(time.Now().Add(budget)); err == nil {
		defer func() { _ = os.Stdin.SetReadDeadline(time.Time{}) }()
	}
	body, err := io.ReadAll(io.LimitReader(os.Stdin, hookMaxBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > hookMaxBody {
		return nil, fmt.Errorf("hook input exceeds 1MiB")
	}
	return body, nil
}

type flexString string

func (f *flexString) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*f = ""
		return nil
	}
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	*f = flexString(strings.TrimSpace(string(data)))
	return nil
}

type flexBool bool

func (f *flexBool) UnmarshalJSON(data []byte) error {
	switch strings.Trim(string(data), `"`) {
	case "true", "1":
		*f = true
	default:
		*f = false
	}
	return nil
}

type claudeHookInput struct {
	SessionID            flexString      `json:"session_id"`
	Cwd                  flexString      `json:"cwd"`
	HookEventName        flexString      `json:"hook_event_name"`
	AgentID              flexString      `json:"agent_id"`
	Prompt               flexString      `json:"prompt"`
	ToolName             flexString      `json:"tool_name"`
	ToolInput            json.RawMessage `json:"tool_input"`
	Message              flexString      `json:"message"`
	NotificationType     flexString      `json:"notification_type"`
	LastAssistantMessage flexString      `json:"last_assistant_message"`
	StopHookActive       flexBool        `json:"stop_hook_active"`
	BackgroundTasks      json.RawMessage `json:"background_tasks"`
	Error                flexString      `json:"error"`
	ErrorDetails         json.RawMessage `json:"error_details"`
	Source               flexString      `json:"source"`
	Reason               flexString      `json:"reason"`
}

func parseClaudeHook(body []byte) (claudeHookInput, error) {
	var in claudeHookInput
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&in); err != nil {
		return claudeHookInput{}, err
	}
	return in, nil
}

// claudeUpdate maps one Claude Code hook payload to a store update.
// ok is false when the event should not be posted.
func claudeUpdate(in claudeHookInput, agent string) (store.Update, bool) {
	sessionID := strings.TrimSpace(string(in.SessionID))
	if sessionID == "" {
		return store.Update{}, false
	}
	if strings.TrimSpace(agent) == "" {
		agent = defaultHookAgent
	}
	jobID, ok := claudeJobID(string(in.Cwd), sessionID)
	if !ok {
		return store.Update{}, false
	}
	event := strings.TrimSpace(string(in.HookEventName))
	subagent := strings.TrimSpace(string(in.AgentID)) != ""
	if subagent && event != "PreToolUse" {
		return store.Update{}, false
	}

	var (
		state    string
		title    string
		hasTitle bool
		note     string
		progress *int
	)
	switch event {
	case "SessionStart":
		state = "queued"
		title = sessionTitle(string(in.Cwd))
		hasTitle = true
		note = withDetail("Session started", string(in.Source))
	case "UserPromptSubmit":
		// A new turn means the earlier API error is no longer how the session ended.
		removeClaudeFailMarker(sessionID)
		state = "running"
		note = "Working on the prompt"
		if t, ok := promptTitle(string(in.Prompt)); ok {
			title = t
			hasTitle = true
		}
	case "PreToolUse":
		state = "running"
		note = toolProgress(string(in.Cwd), string(in.ToolName), toolObject(in.ToolInput))
		if subagent && !strings.HasPrefix(note, "Subagent: ") {
			note = "Subagent: " + note
			note = truncateRunes(collapse(note), hookNoteMax, false)
		}
	case "PostToolUse", "PostToolUseFailure":
		return store.Update{}, false
	case "Notification":
		switch string(in.NotificationType) {
		case "permission_prompt", "elicitation_dialog", "elicitation_url_dialog":
			state = "blocked"
			msg := sanitizeText(string(in.Message), hookNoteMax)
			note = "Needs your input"
			if msg != "" {
				note += ": " + msg
			}
		default:
			return store.Update{}, false
		}
	case "Stop":
		if bool(in.StopHookActive) {
			return store.Update{}, false
		}
		if n := countJSONArray(in.BackgroundTasks); n > 0 {
			state = "running"
			note = fmt.Sprintf("Waiting on %d background task(s)", n)
		} else {
			state = "done"
			hundred := 100
			progress = &hundred
			note = turnFinishedNote(string(in.LastAssistantMessage))
			removeClaudeFailMarker(sessionID)
		}
	case "StopFailure":
		state = "failed"
		note = failureNote(string(in.Error), rawJSONText(in.ErrorDetails))
		writeClaudeFailMarker(sessionID)
	case "SessionEnd":
		if claudeFailMarkerExists(sessionID) {
			state = "failed"
			note = withDetail("Session ended after an API error", string(in.Reason))
			removeClaudeFailMarker(sessionID)
		} else {
			state = "done"
			note = withDetail("Session ended", string(in.Reason))
		}
	default:
		return store.Update{}, false
	}

	note = collapse(redactSecrets(note))
	if hasTitle {
		title = cleanTitle(title)
		if title == "" {
			hasTitle = false
		}
	}
	u := newHookUpdate(agent, jobID, state, title, hasTitle, note, progress)
	if event == "SessionStart" {
		seq := *u.Seq - sessionStartSeqBackdate
		u.Seq = &seq
	}
	return u, true
}

func newHookUpdate(agent, jobID, state, title string, hasTitle bool, note string, progress *int) store.Update {
	seq := nextSeq()
	ttl := hookTTL
	u := store.Update{
		Agent: agent,
		JobID: jobID,
		State: state,
		Seq:   &seq,
		TTL:   &ttl,
	}
	if hasTitle {
		t := title
		u.Title = &t
	}
	if note != "" {
		n := note
		u.Note = &n
	}
	if progress != nil {
		p := *progress
		u.Progress = &p
	}
	return u
}

func claudeFailMarkerPath(sessionID string) (string, bool) {
	slug := slugify(strings.TrimSpace(sessionID), 0)
	if slug == "" {
		return "", false
	}
	dir := claudeFailMarkerDir
	if strings.TrimSpace(dir) == "" {
		dir = filepath.Join(os.TempDir(), "statuspan-claude")
	}
	return filepath.Join(dir, slug+".failed"), true
}

func writeClaudeFailMarker(sessionID string) {
	path, ok := claudeFailMarkerPath(sessionID)
	if !ok {
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	_ = os.Chmod(dir, 0o700)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return
	}
	_ = os.Chmod(path, 0o600)
}

func removeClaudeFailMarker(sessionID string) {
	path, ok := claudeFailMarkerPath(sessionID)
	if !ok {
		return
	}
	_ = os.Remove(path)
}

func claudeFailMarkerExists(sessionID string) bool {
	path, ok := claudeFailMarkerPath(sessionID)
	if !ok {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func claudeJobID(cwd, sessionID string) (string, bool) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", false
	}
	folder := slugify(folderLabel(cwd), 32)
	if folder == "" {
		folder = "dir"
	}
	sess := slugify(sessionID, 0)
	if len(sess) > 8 {
		sess = sess[:8]
	}
	sess = strings.Trim(sess, "-")
	if sess == "" {
		return "", false
	}
	return "claude-" + folder + "-" + sess, true
}

func folderLabel(cwd string) string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return ""
	}
	base := filepath.Base(filepath.Clean(cwd))
	if base == "." || base == string(filepath.Separator) {
		return ""
	}
	return base
}

func sessionTitle(cwd string) string {
	folder := folderLabel(cwd)
	if folder == "" {
		return "Claude Code session"
	}
	return folder + ": Claude Code session"
}

func slugify(s string, max int) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if max > 0 && len(out) > max {
		out = strings.Trim(out[:max], "-")
	}
	return out
}

func withDetail(prefix, detail string) string {
	detail = collapse(detail)
	if detail == "" {
		return prefix
	}
	return prefix + " (" + detail + ")"
}

func promptTitle(prompt string) (string, bool) {
	cleaned := stripMarkup(prompt)
	flat := collapse(cleaned)
	if flat == "" || strings.HasPrefix(flat, "/") || len(strings.Fields(flat)) < 3 {
		return "", false
	}
	line := collapse(firstLine(cleaned))
	if line == "" {
		line = flat
	}
	line = cleanTitle(line)
	if line == "" {
		return "", false
	}
	return line, true
}

func cleanTitle(s string) string {
	return truncateRunes(redactSecrets(collapse(s)), hookTitleMax, true)
}

var (
	pastedBlock = regexp.MustCompile(`(?is)<pasted_content\b[^>]*>.*?</pasted_content\b[^>]*>`)
	xmlTag      = regexp.MustCompile(`<[^>\n]*>`)
)

func stripMarkup(s string) string {
	s = pastedBlock.ReplaceAllString(s, " ")
	if i := strings.Index(strings.ToLower(s), "<pasted_content"); i >= 0 {
		s = s[:i]
	}
	return xmlTag.ReplaceAllString(s, " ")
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line != "" {
			return line
		}
	}
	return ""
}

func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func turnFinishedNote(last string) string {
	line := sanitizeText(firstLine(last), hookNoteMax)
	if line == "" {
		return "Turn finished"
	}
	return "Turn finished: " + line
}

func failureNote(errText, details string) string {
	errText = collapse(errText)
	details = collapse(details)
	note := "API error"
	switch {
	case errText != "" && details != "":
		note += ": " + errText + " " + details
	case errText != "":
		note += ": " + errText
	case details != "":
		note += ": " + details
	}
	return sanitizeText(note, hookNoteMax)
}

func sanitizeText(s string, max int) string {
	s = collapse(redactSecrets(s))
	if max > 0 {
		s = truncateRunes(s, max, false)
	}
	return s
}

func truncateRunes(s string, max int, ellipsis bool) string {
	r := []rune(strings.TrimSpace(s))
	if max <= 0 {
		return ""
	}
	if len(r) <= max {
		return string(r)
	}
	if !ellipsis {
		return string(r[:max])
	}
	if max == 1 {
		return "…"
	}
	limit := max - 1
	cut := limit
	for i := limit; i > 0; i-- {
		if r[i-1] == ' ' {
			cut = i - 1
			break
		}
	}
	for cut > 0 && r[cut-1] == ' ' {
		cut--
	}
	if cut <= 0 {
		cut = limit
	}
	return string(r[:cut]) + "…"
}

func toolObject(raw json.RawMessage) map[string]any {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return map[string]any{}
	}
	return m
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func countJSONArray(raw json.RawMessage) int {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return 0
	}
	return len(arr)
}

func rawJSONText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return ""
		}
		return s
	}
	return string(raw)
}

func toolProgress(cwd, tool string, input map[string]any) string {
	canon := canonicalTool(tool)
	var note string
	switch canon {
	case "Edit", "Write", "MultiEdit", "Read":
		note = joinTool(canon, displayPath(cwd, asString(input["file_path"])))
	case "NotebookEdit":
		path := asString(input["notebook_path"])
		if path == "" {
			path = asString(input["file_path"])
		}
		note = joinTool("NotebookEdit", displayPath(cwd, path))
	case "Bash":
		cmd := sanitizeText(firstLine(asString(input["command"])), 80)
		if cmd == "" {
			note = "Bash"
		} else {
			note = "Bash: " + cmd
		}
	case "Grep", "Glob":
		note = sanitizeText(canon+" "+asString(input["pattern"]), 60)
	case "WebFetch":
		host := urlHost(asString(input["url"]))
		if host == "" {
			note = "WebFetch"
		} else {
			note = "WebFetch " + host
		}
	case "WebSearch":
		note = sanitizeText("WebSearch "+asString(input["query"]), 60)
	case "Task", "Agent":
		desc := collapse(asString(input["description"]))
		if desc == "" {
			note = "Subagent"
		} else {
			note = "Subagent: " + desc
		}
	case "TodoWrite":
		note = "Updating todo list"
	default:
		if mcp, ok := mcpProgress(tool); ok {
			note = mcp
		} else if strings.TrimSpace(tool) == "" {
			note = "Tool"
		} else {
			note = tool
		}
	}
	return sanitizeText(note, hookNoteMax)
}

func joinTool(tool, path string) string {
	if path == "" {
		return tool
	}
	return tool + " " + path
}

func canonicalTool(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "edit":
		return "Edit"
	case "write":
		return "Write"
	case "multiedit":
		return "MultiEdit"
	case "notebookedit":
		return "NotebookEdit"
	case "read":
		return "Read"
	case "bash":
		return "Bash"
	case "grep":
		return "Grep"
	case "glob":
		return "Glob"
	case "webfetch":
		return "WebFetch"
	case "websearch":
		return "WebSearch"
	case "task":
		return "Task"
	case "agent":
		return "Agent"
	case "todowrite":
		return "TodoWrite"
	default:
		return strings.TrimSpace(name)
	}
}

func displayPath(cwd, path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	cleaned := filepath.Clean(path)
	cwd = strings.TrimSpace(cwd)
	if cwd != "" {
		cwd = filepath.Clean(cwd)
		abs := cleaned
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(cwd, cleaned)
		}
		rel, err := filepath.Rel(cwd, abs)
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return rel
		}
	}
	return filepath.Base(cleaned)
}

func urlHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed := raw
	if !strings.Contains(parsed, "://") {
		parsed = "https://" + parsed
	}
	u, err := url.Parse(parsed)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func mcpProgress(name string) (string, bool) {
	parts := strings.SplitN(name, "__", 3)
	if len(parts) != 3 || parts[0] != "mcp" || parts[1] == "" || parts[2] == "" {
		return "", false
	}
	return "MCP " + parts[1] + ": " + parts[2], true
}

var (
	rePEM      = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*?PRIVATE KEY-----.*?-----END [A-Z0-9 ]*?PRIVATE KEY-----`)
	rePEMHead  = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*?PRIVATE KEY-----`)
	reUserinfo = regexp.MustCompile(`(?i)(https?://)[^\s/:@]+:[^\s/@]+@`)
	reKeyValue = regexp.MustCompile(`(?i)\b([A-Za-z0-9_]*(?:authorization|bearer|password|passwd|api[_-]?key|apikey|secret|token|auth))(\s*[=:]\s*)(?:bearer\s+)?[^\s&]+`)
	reBearer   = regexp.MustCompile(`(?i)\bBearer\s+\S+`)
	reQuery    = regexp.MustCompile(`(?i)([?&][A-Za-z0-9_]*(?:authorization|bearer|password|passwd|api[_-]?key|apikey|secret|token|auth)=)[^&#\s]*`)
	reSK       = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`)
	reGH       = regexp.MustCompile(`\b(?:github_pat|ghp|gho|ghs)_[A-Za-z0-9_]{16,}`)
	reSlack    = regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`)
	reAKIA     = regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)
	reAIza     = regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{30,}`)
	reJWT      = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`)
	reHex      = regexp.MustCompile(`\b[0-9a-fA-F]{32,}\b`)
	reB64      = regexp.MustCompile(`(?:^|[^A-Za-z0-9+/])([A-Za-z0-9+/]{32,}={0,2})(?:[^A-Za-z0-9+/=]|$)`)
)

// redactSecrets replaces credential-shaped text with [redacted] and drops
// control characters. It is best-effort and leaves ordinary prose alone.
func redactSecrets(s string) string {
	if s == "" {
		return ""
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = rePEM.ReplaceAllString(s, "[redacted]")
	s = rePEMHead.ReplaceAllString(s, "[redacted]")
	s = reUserinfo.ReplaceAllString(s, "${1}[redacted]@")
	// Query values stop at '&'. Run this before the generic key=value rule
	// so a later parameter such as page=1 is left alone.
	s = reQuery.ReplaceAllString(s, "${1}[redacted]")
	s = reKeyValue.ReplaceAllString(s, "${1}${2}[redacted]")
	s = reBearer.ReplaceAllString(s, "Bearer [redacted]")
	s = reSK.ReplaceAllString(s, "[redacted]")
	s = reGH.ReplaceAllString(s, "[redacted]")
	s = reSlack.ReplaceAllString(s, "[redacted]")
	s = reAKIA.ReplaceAllString(s, "[redacted]")
	s = reAIza.ReplaceAllString(s, "[redacted]")
	s = reJWT.ReplaceAllString(s, "[redacted]")
	s = reHex.ReplaceAllStringFunc(s, func(m string) string {
		if looksLikeHexSecret(m) {
			return "[redacted]"
		}
		return m
	})
	s = replaceSubmatchIf(s, reB64, "[redacted]", looksLikeBase64Secret)
	return s
}

// looksLikeHexSecret reports whether a hex run is a secret rather than a
// repeated letter. A digit is required so a long run of "x" stays visible.
func looksLikeHexSecret(s string) bool {
	if len(s) < 32 {
		return false
	}
	hasDigit := false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F'):
		default:
			return false
		}
	}
	return hasDigit
}

// looksLikeBase64Secret reports whether a standalone base64 run looks like a
// secret. Mixed case is required so a long run of one letter stays visible.
func looksLikeBase64Secret(s string) bool {
	raw := strings.TrimRight(s, "=")
	if len(raw) < 32 {
		return false
	}
	hasUpper := false
	hasLower := false
	for _, r := range raw {
		switch {
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r >= 'a' && r <= 'z':
			hasLower = true
		case (r >= '0' && r <= '9') || r == '+' || r == '/':
		default:
			return false
		}
	}
	return hasUpper && hasLower
}

func replaceSubmatchIf(s string, re *regexp.Regexp, repl string, ok func(string) bool) string {
	idxs := re.FindAllStringSubmatchIndex(s, -1)
	if len(idxs) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, loc := range idxs {
		if len(loc) < 4 || loc[2] < 0 {
			continue
		}
		secret := s[loc[2]:loc[3]]
		if ok != nil && !ok(secret) {
			continue
		}
		b.WriteString(s[last:loc[2]])
		b.WriteString(repl)
		last = loc[3]
	}
	b.WriteString(s[last:])
	return b.String()
}

func hooksCmd(args []string) error {
	if len(args) == 0 || isHelpArg(args[0]) {
		usage()
		return nil
	}
	switch args[0] {
	case "install", "uninstall", "print":
		return hooksAction(args[0], args[1:])
	default:
		return fmt.Errorf("unknown hooks command %q (try statuspan hooks install claude)", args[0])
	}
}

func hooksAction(action string, args []string) error {
	if err := refuseBakedToken(args); err != nil {
		return err
	}
	fs := newFlagSet("hooks " + action)
	project := fs.Bool("project", false, "write ./.claude/settings.json (relative to --dir)")
	settings := fs.String("settings", "", "settings file to edit")
	home := fs.String("home", "", "home directory for ~/.claude/settings.json")
	dir := fs.String("dir", "", "base directory for --project")
	dry := fs.Bool("dry-run", false, "print the resulting JSON and write nothing")
	binFlag := fs.String("bin", "", "statuspan binary path to write into the hook command")
	jobURL := fs.String("url", "", "jobs endpoint baked into the hook command")
	addr := fs.String("addr", "", "server address baked into the hook command")
	relayURL := fs.String("relay", "", "relay URL baked into the hook command")
	workspace := fs.String("workspace", "", "relay workspace baked into the hook command")
	tokenFile := fs.String("token-file", "", "relay token file baked into the hook command")
	rest, foundClaude := peelClaudeArg(args)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if !foundClaude {
		if len(fs.Args()) == 1 && fs.Args()[0] == "claude" {
			foundClaude = true
		}
	} else if len(fs.Args()) != 0 {
		return fmt.Errorf("hooks %s requires claude", action)
	}
	if !foundClaude {
		return fmt.Errorf("hooks %s requires claude", action)
	}
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	bin, err := resolveHookBin(*binFlag)
	if err != nil {
		return err
	}
	command := hookCommand(bin, bakedHookFlags(seen, *jobURL, *addr, *relayURL, *workspace, *tokenFile))
	if action == "print" {
		doc := map[string]any{}
		if err := mergeClaudeHooks(doc, command, bin, false); err != nil {
			return err
		}
		data, err := marshalSettings(doc)
		if err != nil {
			return err
		}
		fmt.Fprint(os.Stdout, string(data))
		return nil
	}
	path, err := claudeSettingsPath(*project, *settings, *home, *dir)
	if err != nil {
		return err
	}
	existed, doc, err := loadSettings(path)
	if err != nil {
		return err
	}
	if err := mergeClaudeHooks(doc, command, bin, action == "uninstall"); err != nil {
		return err
	}
	data, err := marshalSettings(doc)
	if err != nil {
		return err
	}
	if *dry {
		fmt.Fprint(os.Stdout, string(data))
		return nil
	}
	if action == "uninstall" && existed == nil {
		fmt.Fprintf(os.Stderr, "No settings file at %s\n", path)
		return nil
	}
	return writeClaudeSettings(path, data, existed)
}

// peelClaudeArg accepts "claude" before flags (`hooks install claude --project`).
// Flags-then-positional still works: the leftover arg is checked after Parse.
func peelClaudeArg(args []string) ([]string, bool) {
	if len(args) > 0 && args[0] == "claude" {
		return args[1:], true
	}
	return args, false
}

func refuseBakedToken(args []string) error {
	for _, a := range args {
		name := a
		if strings.HasPrefix(a, "--") {
			name = strings.TrimPrefix(a, "--")
		} else if strings.HasPrefix(a, "-") {
			name = strings.TrimPrefix(a, "-")
		} else {
			continue
		}
		name, _, _ = strings.Cut(name, "=")
		if name == "token" {
			return fmt.Errorf("refusing to store --token in Claude settings; use --token-file or STATUSPAN_TOKEN")
		}
	}
	return nil
}

func resolveHookBin(override string) (string, error) {
	override = strings.TrimSpace(override)
	if override != "" {
		return override, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("statuspan binary: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("statuspan binary: %w", err)
	}
	if !filepath.IsAbs(resolved) {
		abs, err := filepath.Abs(resolved)
		if err != nil {
			return "", err
		}
		resolved = abs
	}
	return resolved, nil
}

func bakedHookFlags(seen map[string]bool, jobURL, addr, relayURL, workspace, tokenFile string) []string {
	var out []string
	add := func(name, val string) {
		if !seen[name] {
			return
		}
		out = append(out, "--"+name, quoteArg(val))
	}
	add("url", jobURL)
	add("addr", addr)
	add("relay", relayURL)
	add("workspace", workspace)
	add("token-file", tokenFile)
	return out
}

func hookCommand(bin string, flags []string) string {
	parts := make([]string, 0, 3+len(flags))
	parts = append(parts, quoteArg(bin), "hook", "claude")
	parts = append(parts, flags...)
	return strings.Join(parts, " ")
}

func quoteArg(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n\"'\\") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func claudeSettingsPath(project bool, settings, home, dir string) (string, error) {
	if strings.TrimSpace(settings) != "" {
		return settings, nil
	}
	if project {
		base := strings.TrimSpace(dir)
		if base == "" {
			var err error
			base, err = os.Getwd()
			if err != nil {
				return "", err
			}
		}
		return filepath.Join(base, ".claude", "settings.json"), nil
	}
	h := strings.TrimSpace(home)
	if h == "" {
		var err error
		h, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("home directory: %w", err)
		}
	}
	return filepath.Join(h, ".claude", "settings.json"), nil
}

func loadSettings(path string) ([]byte, map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, map[string]any{}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	doc, err := parseSettings(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return data, doc, nil
}

func parseSettings(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(data)))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("settings file is not valid JSON: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("settings file is not valid JSON")
	}
	if doc == nil {
		return nil, fmt.Errorf("settings file is not a JSON object")
	}
	return doc, nil
}

type hookEventSpec struct {
	name    string
	matcher string
	async   bool
	timeout int
}

func claudeHookSpecs() []hookEventSpec {
	return []hookEventSpec{
		{name: "SessionStart", timeout: 3},
		{name: "UserPromptSubmit", async: true},
		{name: "PreToolUse", matcher: "*", async: true},
		{name: "Notification", matcher: "permission_prompt|elicitation_dialog|elicitation_url_dialog", async: true},
		{name: "Stop", timeout: 5},
		{name: "StopFailure", timeout: 5},
		{name: "SessionEnd", timeout: 3},
	}
}

func mergeClaudeHooks(doc map[string]any, command, bin string, removeOnly bool) error {
	hooks, err := hooksObject(doc)
	if err != nil {
		return err
	}
	if err := stripStatuspanHooks(hooks, bin); err != nil {
		return err
	}
	if !removeOnly {
		for _, spec := range claudeHookSpecs() {
			if err := appendHookGroup(hooks, spec, command); err != nil {
				return err
			}
		}
	}
	if len(hooks) == 0 {
		delete(doc, "hooks")
		return nil
	}
	doc["hooks"] = hooks
	return nil
}

func hooksObject(doc map[string]any) (map[string]any, error) {
	raw, ok := doc["hooks"]
	if !ok || raw == nil {
		return map[string]any{}, nil
	}
	hooks, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("hooks is not a JSON object")
	}
	return hooks, nil
}

func stripStatuspanHooks(hooks map[string]any, bin string) error {
	for event, raw := range hooks {
		groups, err := asArray(raw)
		if err != nil {
			return fmt.Errorf("hooks.%s: %w", event, err)
		}
		kept := make([]any, 0, len(groups))
		for _, g := range groups {
			gm, ok := g.(map[string]any)
			if !ok {
				kept = append(kept, g)
				continue
			}
			hs, exists := gm["hooks"]
			if !exists {
				kept = append(kept, gm)
				continue
			}
			handlers, err := asArray(hs)
			if err != nil {
				return fmt.Errorf("hooks.%s.hooks: %w", event, err)
			}
			hkept := make([]any, 0, len(handlers))
			for _, h := range handlers {
				hm, ok := h.(map[string]any)
				if !ok {
					hkept = append(hkept, h)
					continue
				}
				cmd, _ := hm["command"].(string)
				if isStatuspanClaudeHook(cmd, bin) {
					continue
				}
				hkept = append(hkept, hm)
			}
			if len(hkept) == 0 {
				continue
			}
			gm["hooks"] = hkept
			kept = append(kept, gm)
		}
		if len(kept) == 0 {
			delete(hooks, event)
			continue
		}
		hooks[event] = kept
	}
	return nil
}

func asArray(raw any) ([]any, error) {
	if raw == nil {
		return nil, nil
	}
	groups, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("expected an array")
	}
	return groups, nil
}

func appendHookGroup(hooks map[string]any, spec hookEventSpec, command string) error {
	groups, err := asArray(hooks[spec.name])
	if err != nil {
		return fmt.Errorf("hooks.%s: %w", spec.name, err)
	}
	handler := map[string]any{
		"type":    "command",
		"command": command,
	}
	if spec.async {
		handler["async"] = true
	}
	if spec.timeout > 0 {
		handler["timeout"] = spec.timeout
	}
	group := map[string]any{"hooks": []any{handler}}
	if spec.matcher != "" {
		group["matcher"] = spec.matcher
	}
	hooks[spec.name] = append(groups, group)
	return nil
}

func isStatuspanClaudeHook(command, bin string) bool {
	if !strings.Contains(command, " hook claude") {
		return false
	}
	argv := splitShellWords(command)
	if len(argv) == 0 {
		return false
	}
	base := hookBasename(argv[0])
	if base == "statuspan" {
		return true
	}
	return bin != "" && base == hookBasename(bin)
}

func hookBasename(path string) string {
	base := strings.ToLower(filepath.Base(path))
	base = strings.TrimSuffix(base, ".exe")
	return base
}

func splitShellWords(s string) []string {
	var args []string
	var b strings.Builder
	inWord := false
	flush := func() {
		if !inWord {
			return
		}
		args = append(args, b.String())
		b.Reset()
		inWord = false
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\n':
			flush()
		case '\'':
			inWord = true
			i++
			for i < len(s) && s[i] != '\'' {
				b.WriteByte(s[i])
				i++
			}
		case '"':
			inWord = true
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				b.WriteByte(s[i])
				i++
			}
		case '\\':
			inWord = true
			if i+1 < len(s) {
				i++
				b.WriteByte(s[i])
			}
		default:
			inWord = true
			b.WriteByte(s[i])
		}
	}
	flush()
	return args
}

func marshalSettings(doc map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeClaudeSettings(path string, data, existed []byte) error {
	var backup string
	if existed != nil {
		var err error
		backup, err = writeSettingsBackup(path, existed)
		if err != nil {
			return err
		}
	}
	if err := writeSettingsAtomic(path, data); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Updated %s\n", path)
	if backup != "" {
		fmt.Fprintf(os.Stderr, "Backup %s\n", backup)
	}
	fmt.Fprintln(os.Stderr, "Restart Claude Code sessions to pick up the hooks.")
	return nil
}

func writeSettingsBackup(path string, data []byte) (string, error) {
	dest := path + ".statuspan-backup-" + time.Now().Format("20060102-150405")
	for i := 0; ; i++ {
		name := dest
		if i > 0 {
			name = fmt.Sprintf("%s-%d", dest, i)
		}
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		tmp := f.Name()
		if _, err := f.Write(data); err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return "", err
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(tmp)
			return "", err
		}
		return tmp, nil
	}
}

func writeSettingsAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := mkdirAll0755(dir); err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(dir, ".settings-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}
