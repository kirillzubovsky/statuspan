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

// claudeFailMarkerDir holds one empty file per Claude session that ended a
// turn in StopFailure. Each hook event is a new process, so SessionEnd cannot
// see that failure unless it is written down. Tests replace this directory.
var claudeFailMarkerDir = filepath.Join(os.TempDir(), "statuspan-claude")

// grokFailMarkerDir is the same marker for Grok sessions. Tests replace it.
var grokFailMarkerDir = filepath.Join(os.TempDir(), "statuspan-grok")

// hookAdapter is one coding tool that can report through `statuspan hook`.
// Stdout is per tool: Claude and Grok print nothing, because those CLIs feed
// hook stdout back into the session. A later tool can set stdout to "{}" or
// `{"continue":true}`.
type hookAdapter struct {
	name               string
	displayAgent       string
	jobPrefix          string
	markerDir          func() string
	ignoreShutdownStop bool
	mapStopCancelled   bool
	specs              []hookEventSpec
	stdout             string
	locate             func(project bool, settings, home, dir string) (string, error)
	// file is true when install writes one StatusPan-owned JSON file.
	// Claude merges handlers into a settings object instead.
	file bool
}

type hookOpts struct {
	defaultAgent       string
	product            string
	jobPrefix          string
	markerDir          func() string
	ignoreShutdownStop bool
	mapStopCancelled   bool
}

func hookRegistry() []hookAdapter {
	return []hookAdapter{claudeAdapter(), grokAdapter()}
}

func claudeAdapter() hookAdapter {
	return hookAdapter{
		name:         "claude",
		displayAgent: defaultHookAgent,
		jobPrefix:    "claude",
		markerDir:    func() string { return claudeFailMarkerDir },
		specs:        claudeHookSpecs(),
		locate:       claudeSettingsPath,
	}
}

func grokAdapter() hookAdapter {
	return hookAdapter{
		name:               "grok",
		displayAgent:       "Grok",
		jobPrefix:          "grok",
		markerDir:          func() string { return grokFailMarkerDir },
		ignoreShutdownStop: true,
		mapStopCancelled:   true,
		specs:              grokHookSpecs(),
		locate:             grokHooksPath,
		file:               true,
	}
}

func (a hookAdapter) hookOpts() hookOpts {
	return hookOpts{
		defaultAgent:       a.displayAgent,
		product:            a.displayAgent,
		jobPrefix:          a.jobPrefix,
		markerDir:          a.markerDir,
		ignoreShutdownStop: a.ignoreShutdownStop,
		mapStopCancelled:   a.mapStopCancelled,
	}
}

func lookupHookAdapter(name string) (hookAdapter, bool) {
	for _, a := range hookRegistry() {
		if a.name == name {
			return a, true
		}
	}
	return hookAdapter{}, false
}

func supportedHookTools() string {
	names := make([]string, 0, len(hookRegistry()))
	for _, a := range hookRegistry() {
		names = append(names, a.name)
	}
	return strings.Join(names, ", ")
}

func unknownHookTool(name string) error {
	supported := supportedHookTools()
	if strings.TrimSpace(name) == "" {
		return &exitError{code: 2, err: fmt.Errorf("unknown tool (supported: %s)", supported)}
	}
	return &exitError{code: 2, err: fmt.Errorf("unknown tool %q (supported: %s)", name, supported)}
}

// hookCmd is `statuspan hook <tool>`. An unknown tool exits 0 and prints
// nothing so a mis-installed hook cannot break the agent. Grok also executes
// Claude and Cursor hook files; GROK_HOOK_EVENT means this process is Grok
// even when the command says `hook claude`.
func hookCmd(args []string) error {
	if len(args) == 0 || isHelpArg(args[0]) {
		usage()
		return nil
	}
	invoked := args[0]
	adapter, ok := lookupHookAdapter(invoked)
	if strings.TrimSpace(os.Getenv("GROK_HOOK_EVENT")) != "" {
		if g, gok := lookupHookAdapter("grok"); gok {
			adapter = g
			ok = true
		}
	}
	if !ok {
		return nil
	}
	return runHookAdapter(adapter, invoked, args[1:])
}

func runHookAdapter(adapter hookAdapter, invoked string, args []string) error {
	fs := newFlagSet("hook " + invoked)
	agent := fs.String("agent", envOr("STATUSPAN_AGENT", adapter.displayAgent), "agent name (default STATUSPAN_AGENT or the tool's display name)")
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
	if err := postHook(adapter, seen, strings.TrimSpace(*agent), *jobURL, *addr, *relayURL, *workspace, *token, *tokenFile); err != nil {
		warn(err)
	}
	if adapter.stdout != "" {
		fmt.Fprint(os.Stdout, adapter.stdout)
	}
	return nil
}

func postHook(adapter hookAdapter, seen map[string]bool, agent, jobURL, addr, relayURL, workspace, token, tokenFile string) error {
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
		agent = adapter.displayAgent
	}
	update, ok := adapter.update(in, agent)
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
	SessionID               flexString      `json:"session_id"`
	SessionIDAlt            flexString      `json:"sessionId"`
	Cwd                     flexString      `json:"cwd"`
	WorkspaceRoot           flexString      `json:"workspaceRoot"`
	HookEventName           flexString      `json:"hook_event_name"`
	HookEventNameAlt        flexString      `json:"hookEventName"`
	AgentID                 flexString      `json:"agent_id"`
	AgentIDAlt              flexString      `json:"agentId"`
	Prompt                  flexString      `json:"prompt"`
	ToolName                flexString      `json:"tool_name"`
	ToolNameAlt             flexString      `json:"toolName"`
	ToolInput               json.RawMessage `json:"tool_input"`
	ToolInputAlt            json.RawMessage `json:"toolInput"`
	Message                 flexString      `json:"message"`
	NotificationType        flexString      `json:"notification_type"`
	NotificationTypeAlt     flexString      `json:"notificationType"`
	LastAssistantMessage    flexString      `json:"last_assistant_message"`
	LastAssistantMessageAlt flexString      `json:"lastAssistantMessage"`
	StopHookActive          flexBool        `json:"stop_hook_active"`
	StopHookActiveAlt       flexBool        `json:"stopHookActive"`
	BackgroundTasks         json.RawMessage `json:"background_tasks"`
	BackgroundTasksAlt      json.RawMessage `json:"backgroundTasks"`
	Error                   flexString      `json:"error"`
	ErrorDetails            json.RawMessage `json:"error_details"`
	ErrorDetailsAlt         json.RawMessage `json:"errorDetails"`
	Source                  flexString      `json:"source"`
	Reason                  flexString      `json:"reason"`
}

func parseClaudeHook(body []byte) (claudeHookInput, error) {
	var in claudeHookInput
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&in); err != nil {
		return claudeHookInput{}, err
	}
	return in, nil
}

// normalizeHookInput fills snake_case fields from camelCase when the snake
// key is missing. Grok sends both. hook_event_name is PascalCase
// ("PreToolUse"); hookEventName is snake_case ("pre_tool_use").
func normalizeHookInput(in claudeHookInput) claudeHookInput {
	in.SessionID = firstFlex(in.SessionID, in.SessionIDAlt)
	in.Cwd = firstFlex(in.Cwd, in.WorkspaceRoot)
	if strings.TrimSpace(string(in.HookEventName)) == "" {
		in.HookEventName = flexString(normalizeEventName(string(in.HookEventNameAlt)))
	}
	in.AgentID = firstFlex(in.AgentID, in.AgentIDAlt)
	in.ToolName = firstFlex(in.ToolName, in.ToolNameAlt)
	if rawEmpty(in.ToolInput) {
		in.ToolInput = in.ToolInputAlt
	}
	in.NotificationType = firstFlex(in.NotificationType, in.NotificationTypeAlt)
	in.LastAssistantMessage = firstFlex(in.LastAssistantMessage, in.LastAssistantMessageAlt)
	if !bool(in.StopHookActive) {
		in.StopHookActive = in.StopHookActiveAlt
	}
	if rawEmpty(in.BackgroundTasks) {
		in.BackgroundTasks = in.BackgroundTasksAlt
	}
	if rawEmpty(in.ErrorDetails) {
		in.ErrorDetails = in.ErrorDetailsAlt
	}
	return in
}

func firstFlex(primary, fallback flexString) flexString {
	if strings.TrimSpace(string(primary)) != "" {
		return primary
	}
	return fallback
}

func normalizeEventName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if strings.Contains(name, "_") || name == strings.ToLower(name) {
		return snakeToPascal(name)
	}
	return name
}

func snakeToPascal(s string) string {
	var b strings.Builder
	for _, part := range strings.Split(s, "_") {
		if part == "" {
			continue
		}
		r := []rune(strings.ToLower(part))
		r[0] = unicode.ToUpper(r[0])
		b.WriteString(string(r))
	}
	return b.String()
}

// claudeUpdate maps one Claude Code hook payload to a store update.
// ok is false when the event should not be posted.
func claudeUpdate(in claudeHookInput, agent string) (store.Update, bool) {
	return claudeAdapter().update(in, agent)
}

// grokUpdate maps one Grok CLI hook payload. It follows Claude, and also
// ignores a shutdown Stop and records StopCancelled.
func grokUpdate(in claudeHookInput, agent string) (store.Update, bool) {
	return grokAdapter().update(in, agent)
}

func (a hookAdapter) update(in claudeHookInput, agent string) (store.Update, bool) {
	return mapSessionHook(normalizeHookInput(in), agent, a.hookOpts())
}

// mapSessionHook maps one Claude-shaped hook payload. ok is false when the
// event should not be posted.
func mapSessionHook(in claudeHookInput, agent string, opts hookOpts) (store.Update, bool) {
	sessionID := strings.TrimSpace(string(in.SessionID))
	if sessionID == "" {
		return store.Update{}, false
	}
	if strings.TrimSpace(agent) == "" {
		agent = opts.defaultAgent
	}
	jobID, ok := hookJobID(opts.jobPrefix, string(in.Cwd), sessionID)
	if !ok {
		return store.Update{}, false
	}
	event := strings.TrimSpace(string(in.HookEventName))
	subagent := strings.TrimSpace(string(in.AgentID)) != ""
	if subagent && event != "PreToolUse" {
		return store.Update{}, false
	}
	markerDir := ""
	if opts.markerDir != nil {
		markerDir = opts.markerDir()
	}
	if strings.TrimSpace(markerDir) == "" {
		prefix := opts.jobPrefix
		if prefix == "" {
			prefix = "claude"
		}
		markerDir = filepath.Join(os.TempDir(), "statuspan-"+prefix)
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
		title = sessionTitle(string(in.Cwd), opts.product)
		hasTitle = true
		note = withDetail("Session started", string(in.Source))
	case "UserPromptSubmit":
		// A new turn means the earlier API error is no longer how the session ended.
		removeFailMarker(markerDir, sessionID)
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
	case "PostToolUse", "PostToolUseFailure", "PermissionDenied":
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
		// Grok fires one more Stop after SessionEnd, with reason shutdown and
		// no assistant message. That is not a turn finishing.
		if opts.ignoreShutdownStop && strings.EqualFold(collapse(string(in.Reason)), "shutdown") {
			return store.Update{}, false
		}
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
			removeFailMarker(markerDir, sessionID)
		}
	case "StopFailure":
		state = "failed"
		note = failureNote(string(in.Error), rawJSONText(in.ErrorDetails))
		writeFailMarker(markerDir, sessionID)
	case "StopCancelled":
		if !opts.mapStopCancelled {
			return store.Update{}, false
		}
		removeFailMarker(markerDir, sessionID)
		state = "done"
		note = withDetail("Turn stopped", string(in.Reason))
	case "SessionEnd":
		if failMarkerExists(markerDir, sessionID) {
			state = "failed"
			note = withDetail("Session ended after an API error", string(in.Reason))
			removeFailMarker(markerDir, sessionID)
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

func failMarkerPath(dir, sessionID string) (string, bool) {
	slug := slugify(strings.TrimSpace(sessionID), 0)
	if slug == "" {
		return "", false
	}
	if strings.TrimSpace(dir) == "" {
		dir = filepath.Join(os.TempDir(), "statuspan-claude")
	}
	return filepath.Join(dir, slug+".failed"), true
}

func writeFailMarker(dir, sessionID string) {
	path, ok := failMarkerPath(dir, sessionID)
	if !ok {
		return
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return
	}
	_ = os.Chmod(parent, 0o700)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return
	}
	_ = os.Chmod(path, 0o600)
}

func removeFailMarker(dir, sessionID string) {
	path, ok := failMarkerPath(dir, sessionID)
	if !ok {
		return
	}
	_ = os.Remove(path)
}

func failMarkerExists(dir, sessionID string) bool {
	path, ok := failMarkerPath(dir, sessionID)
	if !ok {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func claudeJobID(cwd, sessionID string) (string, bool) {
	return hookJobID("claude", cwd, sessionID)
}

func hookJobID(prefix, cwd, sessionID string) (string, bool) {
	sessionID = strings.TrimSpace(sessionID)
	prefix = strings.TrimSpace(prefix)
	if sessionID == "" || prefix == "" {
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
	return prefix + "-" + folder + "-" + sess, true
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

func sessionTitle(cwd, product string) string {
	product = strings.TrimSpace(product)
	if product == "" {
		product = defaultHookAgent
	}
	folder := folderLabel(cwd)
	if folder == "" {
		return product + " session"
	}
	return folder + ": " + product + " session"
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

func rawEmpty(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) == 0 || bytes.Equal(raw, []byte("null"))
}

func toolObject(raw json.RawMessage) map[string]any {
	if rawEmpty(raw) {
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
	// Other CLIs use snake_case names. Claude's own names stay on the switch
	// below, including Glob (not List) and Bash (not "Run:").
	if label, ok := friendlyVerb(strings.TrimSpace(tool)); ok {
		return sanitizeText(genericProgress(cwd, label, input), hookNoteMax)
	}
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
		note = commandNote("Bash", asString(input["command"]))
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
		} else if n, ok := genericFields(cwd, strings.TrimSpace(tool), input); ok {
			note = n
		} else if strings.TrimSpace(tool) == "" {
			note = "Tool"
		} else {
			note = tool
		}
	}
	return sanitizeText(note, hookNoteMax)
}

// friendlyVerb maps non-Claude tool names to a short verb. Claude's PascalCase
// names are handled separately so "Glob" stays Glob and "Bash" stays Bash.
// The lowercase tool name "glob" is List.
func friendlyVerb(name string) (string, bool) {
	if name == "glob" {
		return "List", true
	}
	switch strings.ToLower(name) {
	case "read_file":
		return "Read", true
	case "search_replace", "edit_file", "replace":
		return "Edit", true
	case "write_file", "create_file":
		return "Write", true
	case "run_terminal_command", "run_shell_command", "shell", "exec_command":
		return "Run", true
	case "grep_search", "search_file_content":
		return "Grep", true
	case "list_dir", "list_directory":
		return "List", true
	case "web_fetch":
		return "WebFetch", true
	case "web_search", "google_web_search":
		return "WebSearch", true
	default:
		return "", false
	}
}

func genericProgress(cwd, label string, input map[string]any) string {
	if note, ok := genericFields(cwd, label, input); ok {
		return note
	}
	if strings.TrimSpace(label) == "" {
		return "Tool"
	}
	return label
}

// genericFields formats a tool call from common input keys. A command string
// is "Run: ..." except for Claude's Bash, which never reaches this helper.
func genericFields(cwd, label string, input map[string]any) (string, bool) {
	if cmd, ok := inputString(input, "command"); ok && strings.TrimSpace(cmd) != "" {
		return commandNote("Run", cmd), true
	}
	if path, ok := firstInputString(input, "file_path", "target_file", "path", "absolute_path", "notebook_path"); ok && strings.TrimSpace(path) != "" {
		return joinTool(toolLabel(label), displayPath(cwd, path)), true
	}
	if v, ok := firstInputString(input, "pattern", "query"); ok && strings.TrimSpace(v) != "" {
		return sanitizeText(toolLabel(label)+" "+v, 60), true
	}
	if raw, ok := inputString(input, "url"); ok && strings.TrimSpace(raw) != "" {
		host := urlHost(raw)
		if host == "" {
			return toolLabel(label), true
		}
		return joinTool(toolLabel(label), host), true
	}
	return "", false
}

func toolLabel(label string) string {
	label = strings.TrimSpace(label)
	if label == "" {
		return "Tool"
	}
	return label
}

func commandNote(prefix, command string) string {
	cmd := sanitizeText(firstLine(command), 80)
	if cmd == "" {
		return prefix
	}
	return prefix + ": " + cmd
}

func inputString(input map[string]any, key string) (string, bool) {
	v, ok := input[key]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	return s, true
}

func firstInputString(input map[string]any, keys ...string) (string, bool) {
	for _, key := range keys {
		s, ok := inputString(input, key)
		if ok && strings.TrimSpace(s) != "" {
			return s, true
		}
	}
	return "", false
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
	project := fs.Bool("project", false, "write the project hooks file (relative to --dir)")
	settings := fs.String("settings", "", "settings file to edit")
	home := fs.String("home", "", "home directory (default is the user home)")
	dir := fs.String("dir", "", "base directory for --project")
	dry := fs.Bool("dry-run", false, "print the resulting JSON and write nothing")
	binFlag := fs.String("bin", "", "statuspan binary path to write into the hook command")
	jobURL := fs.String("url", "", "jobs endpoint baked into the hook command")
	addr := fs.String("addr", "", "server address baked into the hook command")
	relayURL := fs.String("relay", "", "relay URL baked into the hook command")
	workspace := fs.String("workspace", "", "relay workspace baked into the hook command")
	tokenFile := fs.String("token-file", "", "relay token file baked into the hook command")
	rest, toolName := peelToolArg(args)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if toolName == "" {
		if len(fs.Args()) == 1 {
			toolName = fs.Args()[0]
		} else if len(fs.Args()) > 1 {
			return fmt.Errorf("unexpected argument %q", fs.Args()[1])
		}
	} else if len(fs.Args()) != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Args()[0])
	}
	adapter, ok := lookupHookAdapter(toolName)
	if !ok {
		return unknownHookTool(toolName)
	}
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	bin, err := resolveHookBin(*binFlag)
	if err != nil {
		return err
	}
	command := hookCommand(bin, adapter.name, bakedHookFlags(seen, *jobURL, *addr, *relayURL, *workspace, *tokenFile))
	if adapter.file {
		return grokHooksAction(action, adapter.specs, command, bin, *project, *settings, *home, *dir, *dry)
	}
	return claudeHooksAction(action, adapter, command, bin, *project, *settings, *home, *dir, *dry)
}

func claudeHooksAction(action string, adapter hookAdapter, command, bin string, project bool, settings, home, dir string, dry bool) error {
	if action == "print" {
		doc := map[string]any{}
		if err := mergeClaudeHooks(doc, adapter.specs, command, bin, false); err != nil {
			return err
		}
		data, err := marshalSettings(doc)
		if err != nil {
			return err
		}
		fmt.Fprint(os.Stdout, string(data))
		return nil
	}
	path, err := adapter.locate(project, settings, home, dir)
	if err != nil {
		return err
	}
	existed, doc, err := loadSettings(path)
	if err != nil {
		return err
	}
	if err := mergeClaudeHooks(doc, adapter.specs, command, bin, action == "uninstall"); err != nil {
		return err
	}
	data, err := marshalSettings(doc)
	if err != nil {
		return err
	}
	if dry {
		fmt.Fprint(os.Stdout, string(data))
		return nil
	}
	if action == "uninstall" && existed == nil {
		fmt.Fprintf(os.Stderr, "No settings file at %s\n", path)
		return nil
	}
	return writeClaudeSettings(path, data, existed)
}

// peelToolArg accepts the tool before flags (`hooks install claude --project`).
// Flags-then-positional still works: the leftover arg is checked after Parse.
func peelToolArg(args []string) ([]string, string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[1:], args[0]
	}
	return args, ""
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
			return fmt.Errorf("refusing to store --token in hook settings; use --token-file or STATUSPAN_TOKEN")
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

func hookCommand(bin, tool string, flags []string) string {
	parts := make([]string, 0, 3+len(flags))
	parts = append(parts, quoteArg(bin), "hook", tool)
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

func grokHookSpecs() []hookEventSpec {
	return []hookEventSpec{
		{name: "SessionStart", timeout: 3},
		{name: "UserPromptSubmit", timeout: 3},
		{name: "PreToolUse", matcher: "*", timeout: 3},
		{name: "Notification", matcher: "permission_prompt", timeout: 3},
		{name: "Stop", timeout: 5},
		{name: "StopFailure", timeout: 5},
		{name: "StopCancelled", timeout: 5},
		{name: "SessionEnd", timeout: 3},
	}
}

func grokHooksPath(project bool, settings, home, dir string) (string, error) {
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
		return filepath.Join(base, ".grok", "hooks", "statuspan.json"), nil
	}
	h := strings.TrimSpace(home)
	if h == "" {
		var err error
		h, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("home directory: %w", err)
		}
	}
	return filepath.Join(h, ".grok", "hooks", "statuspan.json"), nil
}

func grokHooksAction(action string, specs []hookEventSpec, command, bin string, project bool, settings, home, dir string, dry bool) error {
	doc := map[string]any{}
	if err := mergeOwnedHooks(doc, specs, command); err != nil {
		return err
	}
	data, err := marshalSettings(doc)
	if err != nil {
		return err
	}
	if action == "print" {
		fmt.Fprint(os.Stdout, string(data))
		if project {
			fmt.Fprintln(os.Stderr, grokProjectTrustHint)
		}
		return nil
	}
	path, err := grokHooksPath(project, settings, home, dir)
	if err != nil {
		return err
	}
	existed, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && !grokFileOwned(existed, bin) {
		if action == "uninstall" {
			return fmt.Errorf("refusing to remove %s: not a StatusPan hook file", path)
		}
		return fmt.Errorf("refusing to overwrite %s: not a StatusPan hook file", path)
	}
	missing := errors.Is(err, os.ErrNotExist)
	if dry {
		if action == "uninstall" {
			if missing {
				fmt.Fprintf(os.Stderr, "No hooks file at %s\n", path)
			}
			return nil
		}
		fmt.Fprint(os.Stdout, string(data))
		if project {
			fmt.Fprintln(os.Stderr, grokProjectTrustHint)
		}
		return nil
	}
	if action == "uninstall" {
		if missing {
			fmt.Fprintf(os.Stderr, "No hooks file at %s\n", path)
			return nil
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Removed %s\n", path)
		return nil
	}
	var backup string
	if !missing {
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
	if project {
		fmt.Fprintln(os.Stderr, grokProjectTrustHint)
	}
	return nil
}

const grokProjectTrustHint = "Project hooks in Grok need folder trust (`/hooks-trust` or `--trust`)."

func mergeOwnedHooks(doc map[string]any, specs []hookEventSpec, command string) error {
	hooks := map[string]any{}
	for _, spec := range specs {
		if err := appendHookGroup(hooks, spec, command); err != nil {
			return err
		}
	}
	doc["hooks"] = hooks
	return nil
}

func grokFileOwned(data []byte, bin string) bool {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return false
	}
	return walkStatuspanHook(v, "grok", bin)
}

func walkStatuspanHook(v any, tool, bin string) bool {
	switch t := v.(type) {
	case map[string]any:
		if cmd, ok := t["command"].(string); ok && isStatuspanHook(cmd, tool, bin) {
			return true
		}
		for _, child := range t {
			if walkStatuspanHook(child, tool, bin) {
				return true
			}
		}
	case []any:
		for _, child := range t {
			if walkStatuspanHook(child, tool, bin) {
				return true
			}
		}
	}
	return false
}

func mergeClaudeHooks(doc map[string]any, specs []hookEventSpec, command, bin string, removeOnly bool) error {
	hooks, err := hooksObject(doc)
	if err != nil {
		return err
	}
	if err := stripStatuspanHooks(hooks, bin); err != nil {
		return err
	}
	if !removeOnly {
		for _, spec := range specs {
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
	return isStatuspanHook(command, "claude", bin)
}

func isStatuspanHook(command, tool, bin string) bool {
	if !strings.Contains(command, " hook "+tool) {
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
