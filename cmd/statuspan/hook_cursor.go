package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kirillzubovsky/statuspan/internal/store"
)

// cursorHookSpecs are the events StatusPan registers. Permission hooks
// (preToolUse, beforeShellExecution, beforeMCPExecution, beforeReadFile,
// subagentStart) are omitted on purpose: invalid JSON blocks them, and an
// allow reply can pre-approve a tool. No matcher is set.
func cursorHookSpecs() []hookEventSpec {
	return []hookEventSpec{
		{name: "sessionStart", timeout: 3},
		{name: "beforeSubmitPrompt", timeout: 3},
		{name: "postToolUse", timeout: 3},
		{name: "afterFileEdit", timeout: 3},
		{name: "stop", timeout: 5},
		{name: "sessionEnd", timeout: 3},
	}
}

// cursorHooksPath is ~/.cursor/hooks.json. --settings wins. --project writes
// <dir>/.cursor/hooks.json. --home writes <home>/.cursor/hooks.json.
func cursorHooksPath(project bool, settings, home, dir string) (string, error) {
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
		return filepath.Join(base, ".cursor", "hooks.json"), nil
	}
	h := strings.TrimSpace(home)
	if h == "" {
		var err error
		h, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("home directory: %w", err)
		}
	}
	return filepath.Join(h, ".cursor", "hooks.json"), nil
}

func cursorHooksAction(action string, adapter hookAdapter, command, bin string, project bool, settings, home, dir string, dry bool) error {
	if action == "print" {
		doc := map[string]any{}
		if err := ensureCursorVersion(doc); err != nil {
			return err
		}
		if err := mergeCursorHooks(doc, adapter.specs, command, adapter.name, bin, false); err != nil {
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
	if err := ensureCursorVersion(doc); err != nil {
		return err
	}
	if err := mergeCursorHooks(doc, adapter.specs, command, adapter.name, bin, action == "uninstall"); err != nil {
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
		fmt.Fprintf(os.Stderr, "No hooks file at %s\n", path)
		return nil
	}
	return writeClaudeSettings(path, data, existed, hookWriteNotes(adapter, action, project))
}

// ensureCursorVersion keeps version 1. A missing version is set. Any other
// number is refused so a future schema is left untouched.
func ensureCursorVersion(doc map[string]any) error {
	raw, ok := doc["version"]
	if !ok || raw == nil {
		doc["version"] = 1
		return nil
	}
	n, ok := integerVersion(raw)
	if !ok || n != 1 {
		return fmt.Errorf("refusing to edit hooks.json: version %v is not 1", raw)
	}
	return nil
}

func integerVersion(v any) (int, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err == nil {
			return int(i), true
		}
		f, ferr := n.Float64()
		if ferr != nil || f != float64(int64(f)) {
			return 0, false
		}
		return int(f), true
	case float64:
		if n != float64(int64(n)) {
			return 0, false
		}
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	default:
		return 0, false
	}
}

// mergeCursorHooks edits Cursor's flat hooks object. Each event is an array
// of {command, timeout} entries, not Claude's matcher groups. Empty event
// arrays are dropped. The file itself is kept by the caller.
func mergeCursorHooks(doc map[string]any, specs []hookEventSpec, command, tool, bin string, removeOnly bool) error {
	hooks, err := hooksObject(doc)
	if err != nil {
		return err
	}
	if err := stripFlatStatuspanHooks(hooks, tool, bin); err != nil {
		return err
	}
	if !removeOnly {
		for _, spec := range specs {
			if err := appendFlatHook(hooks, spec, command); err != nil {
				return err
			}
		}
	}
	doc["hooks"] = hooks
	return nil
}

func stripFlatStatuspanHooks(hooks map[string]any, tool, bin string) error {
	for event, raw := range hooks {
		items, err := asArray(raw)
		if err != nil {
			return fmt.Errorf("hooks.%s: %w", event, err)
		}
		kept, err := stripFlatHookItems(items, tool, bin)
		if err != nil {
			return fmt.Errorf("hooks.%s: %w", event, err)
		}
		if len(kept) == 0 {
			delete(hooks, event)
			continue
		}
		hooks[event] = kept
	}
	return nil
}

func stripFlatHookItems(items []any, tool, bin string) ([]any, error) {
	kept := make([]any, 0, len(items))
	for _, item := range items {
		hm, ok := item.(map[string]any)
		if !ok {
			kept = append(kept, item)
			continue
		}
		if cmd, _ := hm["command"].(string); isStatuspanHook(cmd, tool, bin) {
			continue
		}
		if raw, exists := hm["hooks"]; exists {
			nested, err := asArray(raw)
			if err != nil {
				return nil, fmt.Errorf("expected an array")
			}
			inner, err := stripFlatHookItems(nested, tool, bin)
			if err != nil {
				return nil, err
			}
			if len(inner) == 0 {
				continue
			}
			hm["hooks"] = inner
		}
		kept = append(kept, hm)
	}
	return kept, nil
}

func appendFlatHook(hooks map[string]any, spec hookEventSpec, command string) error {
	items, err := asArray(hooks[spec.name])
	if err != nil {
		return fmt.Errorf("hooks.%s: %w", spec.name, err)
	}
	handler := map[string]any{"command": command}
	if spec.timeout > 0 {
		handler["timeout"] = spec.timeout
	}
	hooks[spec.name] = append(items, handler)
	return nil
}

// cursorStdout prints {"continue":true} for beforeSubmitPrompt and when the
// payload cannot be read, so a prompt is never blocked. Every recognized
// other event prints {} and never a followup_message.
func cursorStdout(body []byte) string {
	event, parsed := cursorParsedEvent(body)
	if !parsed || strings.EqualFold(event, "beforeSubmitPrompt") {
		return cursorContinueStdout
	}
	return cursorEmptyStdout
}

func cursorParsedEvent(body []byte) (string, bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", false
	}
	in, err := parseClaudeHook(trimmed)
	if err != nil {
		return "", false
	}
	in = normalizeHookInput(in)
	return strings.TrimSpace(string(in.HookEventName)), true
}

// mapCursorHook maps one Cursor agent hook. ok is false when nothing is posted.
func mapCursorHook(in claudeHookInput, agent string, opts hookOpts) (store.Update, bool) {
	sessionID := cursorSessionID(in)
	if sessionID == "" {
		return store.Update{}, false
	}
	if strings.TrimSpace(agent) == "" {
		agent = opts.defaultAgent
	}
	dir := cursorWorkDir(in)
	jobID, ok := hookJobID(opts.jobPrefix, dir, sessionID)
	if !ok {
		return store.Update{}, false
	}
	event, known := cursorCanonicalEvent(string(in.HookEventName))
	if !known {
		return store.Update{}, false
	}
	markerDir := cursorMarkerDir(opts)

	var (
		state    string
		title    string
		hasTitle bool
		note     string
		progress *int
	)
	switch event {
	case "sessionStart":
		if strings.EqualFold(collapse(string(in.ComposerMode)), "ask") {
			return store.Update{}, false
		}
		state = "queued"
		title = sessionTitle(dir, opts.product)
		hasTitle = true
		note = withDetail("Session started", string(in.ComposerMode))
	case "beforeSubmitPrompt":
		removeFailMarker(markerDir, sessionID)
		state = "running"
		note = "Working on the prompt"
		if t, ok := promptTitle(string(in.Prompt)); ok {
			title = t
			hasTitle = true
		}
	case "postToolUse":
		state = "running"
		note = cursorToolNote(dir, string(in.ToolName), toolObject(in.ToolInput))
	case "afterFileEdit":
		state = "running"
		note = cursorEditNote(dir, string(in.FilePath))
	case "stop":
		switch strings.ToLower(collapse(string(in.Status))) {
		case "completed":
			removeFailMarker(markerDir, sessionID)
			state = "done"
			hundred := 100
			progress = &hundred
			note = "Turn finished"
		case "aborted":
			removeFailMarker(markerDir, sessionID)
			state = "done"
			note = "Turn stopped"
		case "error":
			state = "failed"
			note = "Turn failed"
			writeFailMarker(markerDir, sessionID)
		default:
			return store.Update{}, false
		}
	case "sessionEnd":
		if strings.EqualFold(collapse(string(in.Reason)), "error") {
			state = "failed"
			note = cursorSessionErrorNote(string(in.ErrorMessage))
			removeFailMarker(markerDir, sessionID)
		} else if failMarkerExists(markerDir, sessionID) {
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
	if event == "sessionStart" {
		seq := *u.Seq - sessionStartSeqBackdate
		u.Seq = &seq
	}
	return u, true
}

func cursorSessionID(in claudeHookInput) string {
	if id := strings.TrimSpace(string(in.ConversationID)); id != "" {
		return id
	}
	return strings.TrimSpace(string(in.SessionID))
}

// cursorWorkDir is the tool cwd, else the first workspace root, else
// CURSOR_PROJECT_DIR, else this process's working directory.
func cursorWorkDir(in claudeHookInput) string {
	if cwd := strings.TrimSpace(string(in.Cwd)); cwd != "" {
		return cwd
	}
	if root := firstJSONString(in.WorkspaceRoots); root != "" {
		return root
	}
	if env := strings.TrimSpace(os.Getenv("CURSOR_PROJECT_DIR")); env != "" {
		return env
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}

func cursorMarkerDir(opts hookOpts) string {
	if opts.markerDir != nil {
		if dir := strings.TrimSpace(opts.markerDir()); dir != "" {
			return dir
		}
	}
	prefix := opts.jobPrefix
	if prefix == "" {
		prefix = "cursor"
	}
	return filepath.Join(os.TempDir(), "statuspan-"+prefix)
}

func cursorCanonicalEvent(name string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "sessionstart":
		return "sessionStart", true
	case "beforesubmitprompt":
		return "beforeSubmitPrompt", true
	case "posttooluse":
		return "postToolUse", true
	case "afterfileedit":
		return "afterFileEdit", true
	case "stop":
		return "stop", true
	case "sessionend":
		return "sessionEnd", true
	default:
		return "", false
	}
}

func cursorSessionErrorNote(msg string) string {
	msg = sanitizeText(msg, 80)
	if msg == "" {
		return "Session ended with an error"
	}
	return "Session ended with an error: " + msg
}

func cursorEditNote(dir, path string) string {
	rel := displayPath(dir, path)
	if rel == "" {
		return "Edit"
	}
	return sanitizeText("Edit "+rel, hookNoteMax)
}

func cursorToolNote(dir, tool string, input map[string]any) string {
	name := strings.TrimSpace(tool)
	switch strings.ToLower(name) {
	case "shell":
		return sanitizeText(commandNote("Run", asString(input["command"])), hookNoteMax)
	case "read", "write", "delete":
		label := cursorToolLabel(name)
		path, _ := firstInputString(input, "path", "file_path", "target_file")
		return sanitizeText(joinTool(label, displayPath(dir, path)), hookNoteMax)
	case "grep":
		pattern, _ := inputString(input, "pattern")
		return sanitizeText(joinTool("Grep", collapse(pattern)), hookNoteMax)
	case "task":
		desc := collapse(asString(input["description"]))
		if desc == "" {
			return "Subagent"
		}
		return sanitizeText("Subagent: "+desc, hookNoteMax)
	default:
		if mcp, ok := cursorMCPName(name); ok {
			return sanitizeText("MCP "+mcp, hookNoteMax)
		}
		if name == "" {
			return "Tool"
		}
		return sanitizeText(name, hookNoteMax)
	}
}

func cursorToolLabel(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "read":
		return "Read"
	case "write":
		return "Write"
	case "delete":
		return "Delete"
	default:
		return strings.TrimSpace(name)
	}
}

func cursorMCPName(name string) (string, bool) {
	const prefix = "mcp:"
	if len(name) < len(prefix) || !strings.EqualFold(name[:len(prefix)], prefix) {
		return "", false
	}
	tool := strings.TrimSpace(name[len(prefix):])
	if tool == "" {
		return "", false
	}
	return tool, true
}

func firstJSONString(raw json.RawMessage) string {
	if rawEmpty(raw) {
		return ""
	}
	var items []string
	if err := json.Unmarshal(raw, &items); err != nil {
		return ""
	}
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item != "" {
			return item
		}
	}
	return ""
}
