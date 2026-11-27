package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kirillzubovsky/statuspan/internal/store"
)

// geminiHookStdout is the only stdout Gemini CLI accepts. Plain text is
// rejected ("Silence is mandatory"). Every path prints this, including
// ignored events, empty stdin, and input that does not parse.
const geminiHookStdout = "{}\n"

// geminiHandlerDescription is stored on each installed handler. Gemini shows
// it next to the hook name in logs.
const geminiHandlerDescription = "Report this event to StatusPan"

// geminiFailMarkerDir holds one empty file per Gemini session that should
// end failed. Gemini has no failure event of its own. BeforeAgent and a
// finished AfterAgent clear the file. SessionEnd reports failed when it is
// still there. Tests replace this directory.
var geminiFailMarkerDir = filepath.Join(os.TempDir(), "statuspan-gemini")

func geminiAdapter() hookAdapter {
	return hookAdapter{
		name:         "gemini",
		displayAgent: "Gemini",
		jobPrefix:    "gemini",
		markerDir:    func() string { return geminiFailMarkerDir },
		gemini:       true,
		specs:        geminiHookSpecs(),
		stdout:       geminiHookStdout,
		locate:       geminiSettingsPath,
	}
}

// geminiHookSpecs are the events StatusPan registers. Timeouts are
// milliseconds. BeforeTool matches every tool name. Other events have no
// matcher. hooksConfig is never written.
func geminiHookSpecs() []hookEventSpec {
	return []hookEventSpec{
		{name: "SessionStart", timeout: 3000, hookName: "statuspan", description: geminiHandlerDescription},
		{name: "BeforeAgent", timeout: 3000, hookName: "statuspan", description: geminiHandlerDescription},
		{name: "BeforeTool", matcher: ".*", timeout: 3000, hookName: "statuspan", description: geminiHandlerDescription},
		{name: "Notification", timeout: 3000, hookName: "statuspan", description: geminiHandlerDescription},
		{name: "AfterAgent", timeout: 5000, hookName: "statuspan", description: geminiHandlerDescription},
		{name: "SessionEnd", timeout: 3000, hookName: "statuspan", description: geminiHandlerDescription},
	}
}

// geminiSettingsPath is ~/.gemini/settings.json. --settings wins. --project
// writes <dir>/.gemini/settings.json. --home writes <home>/.gemini/settings.json.
// The file holds other settings. Install merges the hooks object and leaves
// every other key, including hooksConfig, as it is.
func geminiSettingsPath(project bool, settings, home, dir string) (string, error) {
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
		return filepath.Join(base, ".gemini", "settings.json"), nil
	}
	h := strings.TrimSpace(home)
	if h == "" {
		var err error
		h, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("home directory: %w", err)
		}
	}
	return filepath.Join(h, ".gemini", "settings.json"), nil
}

// geminiUpdate maps one Gemini CLI hook payload.
func geminiUpdate(in claudeHookInput, agent string) (store.Update, bool) {
	return geminiAdapter().update(in, agent)
}

// mapGeminiHook maps one Gemini CLI hook. ok is false when nothing is posted.
func mapGeminiHook(in claudeHookInput, agent string, opts hookOpts) (store.Update, bool) {
	sessionID := strings.TrimSpace(string(in.SessionID))
	if sessionID == "" {
		return store.Update{}, false
	}
	if strings.TrimSpace(agent) == "" {
		agent = opts.defaultAgent
	}
	dir := strings.TrimSpace(string(in.Cwd))
	jobID, ok := hookJobID(opts.jobPrefix, dir, sessionID)
	if !ok {
		return store.Update{}, false
	}
	event := strings.TrimSpace(string(in.HookEventName))
	markerDir := geminiMarkerDir(opts)

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
		title = sessionTitle(dir, opts.product)
		hasTitle = true
		note = withDetail("Session started", string(in.Source))
	case "BeforeAgent":
		removeFailMarker(markerDir, sessionID)
		state = "running"
		note = "Working on the prompt"
		if t, ok := promptTitle(string(in.Prompt)); ok {
			title = t
			hasTitle = true
		}
	case "BeforeTool":
		state = "running"
		note = geminiToolNote(dir, string(in.ToolName), toolObject(in.ToolInput))
	case "Notification":
		if string(in.NotificationType) != "ToolPermission" {
			return store.Update{}, false
		}
		state = "blocked"
		msg := sanitizeText(string(in.Message), hookNoteMax)
		note = "Needs your approval"
		if msg != "" {
			note += ": " + msg
		}
	case "AfterAgent":
		if bool(in.StopHookActive) {
			return store.Update{}, false
		}
		removeFailMarker(markerDir, sessionID)
		state = "done"
		hundred := 100
		progress = &hundred
		note = turnFinishedNote(string(in.PromptResponse))
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

func geminiMarkerDir(opts hookOpts) string {
	if opts.markerDir != nil {
		if dir := strings.TrimSpace(opts.markerDir()); dir != "" {
			return dir
		}
	}
	prefix := opts.jobPrefix
	if prefix == "" {
		prefix = "gemini"
	}
	return filepath.Join(os.TempDir(), "statuspan-"+prefix)
}

// geminiToolNote is the one-line tool text. Built-in names go through the
// shared helper. web_fetch carries a prompt, which is not shown. MCP tools
// are named mcp_<server>_<tool>.
func geminiToolNote(cwd, tool string, input map[string]any) string {
	name := strings.TrimSpace(tool)
	if mcp, ok := geminiMCPName(name); ok {
		return sanitizeText("MCP "+mcp, hookNoteMax)
	}
	if strings.EqualFold(name, "web_fetch") {
		return "WebFetch"
	}
	return toolProgress(cwd, name, input)
}

func geminiMCPName(name string) (string, bool) {
	const prefix = "mcp_"
	if len(name) < len(prefix) || !strings.EqualFold(name[:len(prefix)], prefix) {
		return "", false
	}
	rest := name[len(prefix):]
	server, tool, ok := strings.Cut(rest, "_")
	if !ok || server == "" || tool == "" {
		return "", false
	}
	return server + "_" + tool, true
}
