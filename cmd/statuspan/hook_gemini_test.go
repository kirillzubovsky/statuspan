package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const geminiJob = "gemini-my-project-abcdef12"

func TestGeminiHookEvents(t *testing.T) {
	hundred := 100
	cases := []struct {
		name     string
		in       claudeHookInput
		skip     bool
		state    string
		title    string
		hasTitle bool
		note     string
		progress *int
	}{
		{
			name:     "session start",
			in:       with(ev("SessionStart"), func(in *claudeHookInput) { in.Source = "startup" }),
			state:    "queued",
			hasTitle: true,
			title:    "My Project: Gemini session",
			note:     "Session started (startup)",
		},
		{
			name:     "session start resume",
			in:       with(ev("SessionStart"), func(in *claudeHookInput) { in.Source = "resume" }),
			state:    "queued",
			hasTitle: true,
			title:    "My Project: Gemini session",
			note:     "Session started (resume)",
		},
		{
			name:     "session start clear",
			in:       with(ev("SessionStart"), func(in *claudeHookInput) { in.Source = "clear" }),
			state:    "queued",
			hasTitle: true,
			title:    "My Project: Gemini session",
			note:     "Session started (clear)",
		},
		{
			name:     "before agent",
			in:       with(ev("BeforeAgent"), func(in *claudeHookInput) { in.Prompt = "please fix the login bug" }),
			state:    "running",
			hasTitle: true,
			title:    "please fix the login bug",
			note:     "Working on the prompt",
		},
		{
			name:  "short follow-up omits title",
			in:    with(ev("BeforeAgent"), func(in *claudeHookInput) { in.Prompt = "go ahead" }),
			state: "running",
			note:  "Working on the prompt",
		},
		{
			name:  "slash command omits title",
			in:    with(ev("BeforeAgent"), func(in *claudeHookInput) { in.Prompt = "/review the board" }),
			state: "running",
			note:  "Working on the prompt",
		},
		{
			name: "shell",
			in: with(ev("BeforeTool"), func(in *claudeHookInput) {
				in.ToolName = "run_shell_command"
				in.ToolInput = json.RawMessage(`{"command":"go test ./...\nsecond"}`)
			}),
			state: "running",
			note:  "Run: go test ./...",
		},
		{
			name: "replace",
			in: with(ev("BeforeTool"), func(in *claudeHookInput) {
				in.ToolName = "replace"
				in.ToolInput = json.RawMessage(`{"file_path":"/work/My Project/main.go"}`)
			}),
			state: "running",
			note:  "Edit main.go",
		},
		{
			name: "write file",
			in: with(ev("BeforeTool"), func(in *claudeHookInput) {
				in.ToolName = "write_file"
				in.ToolInput = json.RawMessage(`{"file_path":"/work/My Project/main.go"}`)
			}),
			state: "running",
			note:  "Write main.go",
		},
		{
			name: "read file",
			in: with(ev("BeforeTool"), func(in *claudeHookInput) {
				in.ToolName = "read_file"
				in.ToolInput = json.RawMessage(`{"file_path":"/work/My Project/main.go"}`)
			}),
			state: "running",
			note:  "Read main.go",
		},
		{
			name: "read file absolute path",
			in: with(ev("BeforeTool"), func(in *claudeHookInput) {
				in.ToolName = "read_file"
				in.ToolInput = json.RawMessage(`{"absolute_path":"/work/My Project/main.go"}`)
			}),
			state: "running",
			note:  "Read main.go",
		},
		{
			name: "web search",
			in: with(ev("BeforeTool"), func(in *claudeHookInput) {
				in.ToolName = "google_web_search"
				in.ToolInput = json.RawMessage(`{"query":"statuspan hooks"}`)
			}),
			state: "running",
			note:  "WebSearch statuspan hooks",
		},
		{
			name: "web fetch hides the prompt",
			in: with(ev("BeforeTool"), func(in *claudeHookInput) {
				in.ToolName = "web_fetch"
				in.ToolInput = json.RawMessage(`{"prompt":"fetch https://example.com token=supersecret"}`)
			}),
			state: "running",
			note:  "WebFetch",
		},
		{
			name: "mcp tool",
			in: with(ev("BeforeTool"), func(in *claudeHookInput) {
				in.ToolName = "mcp_github_create_issue"
				in.ToolInput = json.RawMessage(`{"title":"bug"}`)
			}),
			state: "running",
			note:  "MCP github_create_issue",
		},
		{
			name: "tool permission",
			in: with(ev("Notification"), func(in *claudeHookInput) {
				in.NotificationType = "ToolPermission"
				in.Message = "allow the edit"
				in.ErrorDetails = json.RawMessage(`{"tool":"replace","path":"/tmp/secret"}`)
			}),
			state: "blocked",
			note:  "Needs your approval: allow the edit",
		},
		{
			name: "other notification ignored",
			in:   with(ev("Notification"), func(in *claudeHookInput) { in.NotificationType = "Info" }),
			skip: true,
		},
		{
			name:     "after agent",
			in:       with(ev("AfterAgent"), func(in *claudeHookInput) { in.PromptResponse = "hello\nsecond line" }),
			state:    "done",
			note:     "Turn finished: hello",
			progress: &hundred,
		},
		{
			name:     "after agent without a response",
			in:       ev("AfterAgent"),
			state:    "done",
			note:     "Turn finished",
			progress: &hundred,
		},
		{
			name: "after agent stop hook active ignored",
			in:   with(ev("AfterAgent"), func(in *claudeHookInput) { in.StopHookActive = true; in.PromptResponse = "hidden" }),
			skip: true,
		},
		{
			name:  "session end",
			in:    with(ev("SessionEnd"), func(in *claudeHookInput) { in.Reason = "exit" }),
			state: "done",
			note:  "Session ended (exit)",
		},
		{
			name:  "session end logout",
			in:    with(ev("SessionEnd"), func(in *claudeHookInput) { in.Reason = "logout" }),
			state: "done",
			note:  "Session ended (logout)",
		},
		{
			name:  "session end prompt input",
			in:    with(ev("SessionEnd"), func(in *claudeHookInput) { in.Reason = "prompt_input_exit" }),
			state: "done",
			note:  "Session ended (prompt_input_exit)",
		},
		{
			name: "after tool ignored",
			in:   ev("AfterTool"),
			skip: true,
		},
		{
			name: "before model ignored",
			in:   ev("BeforeModel"),
			skip: true,
		},
		{
			name: "after model ignored",
			in:   ev("AfterModel"),
			skip: true,
		},
		{
			name: "before tool selection ignored",
			in:   ev("BeforeToolSelection"),
			skip: true,
		},
		{
			name: "pre compress ignored",
			in:   ev("PreCompress"),
			skip: true,
		},
		{
			name: "claude stop ignored",
			in:   ev("Stop"),
			skip: true,
		},
		{
			name: "empty session ignored",
			in:   with(ev("BeforeAgent"), func(in *claudeHookInput) { in.SessionID = "" }),
			skip: true,
		},
	}
	var prev int64
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useGeminiFailMarkerDir(t)
			got, ok := geminiUpdate(tc.in, "Gemini")
			if tc.skip {
				if ok {
					t.Fatalf("posted %+v", got)
				}
				return
			}
			if !ok {
				t.Fatal("no post")
			}
			if got.Agent != "Gemini" || got.JobID != geminiJob || got.State != tc.state {
				t.Fatalf("identity %+v", got)
			}
			if got.TTL == nil || *got.TTL != hookTTL {
				t.Fatalf("ttl %+v", got.TTL)
			}
			if got.Seq == nil || *got.Seq <= prev {
				t.Fatalf("seq %+v prev %d", got.Seq, prev)
			}
			prev = *got.Seq
			if tc.hasTitle {
				if got.Title == nil || *got.Title != tc.title {
					t.Fatalf("title %#v", got.Title)
				}
			} else if got.Title != nil {
				t.Fatalf("unexpected title %q", *got.Title)
			}
			if noteOf(got) != tc.note {
				t.Fatalf("note %q", noteOf(got))
			}
			if tc.progress == nil {
				if got.Progress != nil {
					t.Fatalf("progress %d", *got.Progress)
				}
			} else if got.Progress == nil || *got.Progress != *tc.progress {
				t.Fatalf("progress %+v", got.Progress)
			}
		})
	}

	got, ok := geminiUpdate(with(ev("SessionEnd"), func(in *claudeHookInput) { in.Reason = "other" }), "")
	if !ok || got.Agent != "Gemini" || got.JobID != geminiJob || noteOf(got) != "Session ended (other)" {
		t.Fatalf("default agent %+v ok %v", got, ok)
	}
}

func TestGeminiToolNotes(t *testing.T) {
	useGeminiFailMarkerDir(t)
	cases := []struct {
		name  string
		tool  string
		input string
		note  string
	}{
		{name: "shell collapsed", tool: "run_shell_command", input: `{"command":"go    test"}`, note: "Run: go test"},
		{name: "shell secret", tool: "run_shell_command", input: `{"command":"curl token=supersecret"}`, note: "Run: curl token=[redacted]"},
		{name: "grep", tool: "search_file_content", input: `{"pattern":"hook gemini"}`, note: "Grep hook gemini"},
		{name: "grep search", tool: "grep_search", input: `{"pattern":"statuspan"}`, note: "Grep statuspan"},
		{name: "list directory", tool: "list_directory", input: `{"path":"/work/My Project/cmd"}`, note: "List cmd"},
		{name: "glob", tool: "glob", input: `{"pattern":"**/*.go"}`, note: "List **/*.go"},
		{name: "mcp rest", tool: "mcp_fs_read_file", input: `{}`, note: "MCP fs_read_file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := with(ev("BeforeTool"), func(in *claudeHookInput) {
				in.ToolName = flexString(tc.tool)
				in.ToolInput = json.RawMessage(tc.input)
			})
			got, ok := geminiUpdate(in, "Gemini")
			if !ok || noteOf(got) != tc.note {
				t.Fatalf("ok %v note %q want %q", ok, noteOf(got), tc.note)
			}
		})
	}
}

func TestGeminiSessionStartSeqBackdated(t *testing.T) {
	useGeminiFailMarkerDir(t)
	start := with(ev("SessionStart"), func(in *claudeHookInput) { in.Source = "startup" })
	before := nextSeq()
	got, ok := geminiUpdate(start, "Gemini")
	after := nextSeq()
	if !ok || got.Seq == nil {
		t.Fatal("session start posted nothing")
	}
	assigned := *got.Seq + sessionStartSeqBackdate
	if assigned <= before || assigned >= after {
		t.Fatalf("session seq %d is not nextSeq()-2000 between %d and %d", *got.Seq, before, after)
	}
	prompt := with(ev("BeforeAgent"), func(in *claudeHookInput) { in.Prompt = "please fix the login bug" })
	next, ok := geminiUpdate(prompt, "Gemini")
	if !ok || next.Seq == nil || *next.Seq <= *got.Seq {
		t.Fatalf("prompt seq %+v start %d", next.Seq, *got.Seq)
	}
}

func TestGeminiFailMarker(t *testing.T) {
	dir := useGeminiFailMarkerDir(t)
	end := with(ev("SessionEnd"), func(in *claudeHookInput) { in.Reason = "exit" })

	writeFailMarker(dir, testSession)
	got, ok := geminiUpdate(end, "Gemini")
	if !ok || got.State != "failed" || noteOf(got) != "Session ended after an API error (exit)" {
		t.Fatalf("session end ok %v state %s note %q", ok, got.State, noteOf(got))
	}
	if failMarkerExists(dir, testSession) {
		t.Fatal("marker remained after session end")
	}

	writeFailMarker(dir, testSession)
	if _, ok := geminiUpdate(with(ev("BeforeAgent"), func(in *claudeHookInput) { in.Prompt = "go ahead" }), "Gemini"); !ok {
		t.Fatal("prompt posted nothing")
	}
	if failMarkerExists(dir, testSession) {
		t.Fatal("before agent left the fail marker")
	}
	got, ok = geminiUpdate(end, "Gemini")
	if !ok || got.State != "done" || noteOf(got) != "Session ended (exit)" {
		t.Fatalf("cleared session end ok %v state %s note %q", ok, got.State, noteOf(got))
	}

	writeFailMarker(dir, testSession)
	got, ok = geminiUpdate(ev("AfterAgent"), "Gemini")
	if !ok || got.State != "done" || noteOf(got) != "Turn finished" || got.Progress == nil || *got.Progress != 100 {
		t.Fatalf("after agent ok %v state %s note %q progress %+v", ok, got.State, noteOf(got), got.Progress)
	}
	if failMarkerExists(dir, testSession) {
		t.Fatal("after agent left the fail marker")
	}

	writeFailMarker(dir, testSession)
	active := with(ev("AfterAgent"), func(in *claudeHookInput) { in.StopHookActive = true })
	if _, ok := geminiUpdate(active, "Gemini"); ok {
		t.Fatal("stop hook active posted")
	}
	if !failMarkerExists(dir, testSession) {
		t.Fatal("stop hook active cleared the marker")
	}
	got, ok = geminiUpdate(end, "Gemini")
	if !ok || got.State != "failed" {
		t.Fatalf("session end after active stop ok %v state %s", ok, got.State)
	}
}

func TestGeminiRedactsPromptTitle(t *testing.T) {
	useGeminiFailMarkerDir(t)
	in := with(ev("BeforeAgent"), func(in *claudeHookInput) {
		in.Prompt = "please rotate token=supersecret now"
	})
	got, ok := geminiUpdate(in, "Gemini")
	if !ok || got.Title == nil || *got.Title != "please rotate token=[redacted] now" {
		t.Fatalf("ok %v title %#v", ok, got.Title)
	}
	blocked := with(ev("Notification"), func(in *claudeHookInput) {
		in.NotificationType = "ToolPermission"
		in.Message = "allow token=supersecret"
	})
	got, ok = geminiUpdate(blocked, "Gemini")
	if !ok || noteOf(got) != "Needs your approval: allow token=[redacted]" {
		t.Fatalf("ok %v note %q", ok, noteOf(got))
	}
}

func TestGeminiHookStdout(t *testing.T) {
	useGeminiFailMarkerDir(t)
	clearProxy(t)
	ts, rec := newPostServer(t)
	handled := `{"session_id":"abcdef1234567890","cwd":"/work/My Project","hook_event_name":"BeforeAgent","prompt":"please fix the login bug"}`
	stdout, stderr, err := runHook(t, handled, "hook", "gemini", "--url", ts.URL+"/api/jobs")
	if err != nil || stdout != "{}\n" || stderr != "" {
		t.Fatalf("handled err %v stdout %q stderr %q", err, stdout, stderr)
	}
	hits := rec.snapshot()
	if len(hits) != 1 || hits[0].body["agent"] != "Gemini" || hits[0].body["job_id"] != geminiJob || hits[0].body["state"] != "running" {
		t.Fatalf("hits %+v", hits)
	}

	ignored := []string{
		`{"session_id":"abcdef1234567890","cwd":"/work/My Project","hook_event_name":"AfterTool"}`,
		`{"session_id":"abcdef1234567890","cwd":"/work/My Project","hook_event_name":"BeforeModel"}`,
		`{"session_id":"abcdef1234567890","cwd":"/work/My Project","hook_event_name":"AfterModel"}`,
		`{"session_id":"abcdef1234567890","cwd":"/work/My Project","hook_event_name":"BeforeToolSelection"}`,
		`{"session_id":"abcdef1234567890","cwd":"/work/My Project","hook_event_name":"PreCompress"}`,
		`{"session_id":"abcdef1234567890","cwd":"/work/My Project","hook_event_name":"Notification","notification_type":"Info","message":"nope"}`,
		`{"session_id":"abcdef1234567890","cwd":"/work/My Project","hook_event_name":"AfterAgent","stop_hook_active":true,"prompt_response":"hidden"}`,
	}
	for _, payload := range ignored {
		before := len(rec.snapshot())
		stdout, stderr, err = runHook(t, payload, "hook", "gemini", "--url", ts.URL+"/api/jobs")
		if err != nil || stdout != "{}\n" || stderr != "" || len(rec.snapshot()) != before {
			t.Fatalf("ignored err %v stdout %q stderr %q payload %s", err, stdout, stderr, payload)
		}
	}

	stdout, stderr, err = runHook(t, "", "hook", "gemini", "--url", ts.URL+"/api/jobs")
	if err != nil || stdout != "{}\n" || stderr != "" || len(rec.snapshot()) != 1 {
		t.Fatalf("empty err %v stdout %q stderr %q hits %d", err, stdout, stderr, len(rec.snapshot()))
	}
	stdout, stderr, err = runHook(t, "{", "hook", "gemini", "--url", ts.URL+"/api/jobs")
	if err != nil || stdout != "{}\n" || stderr != "" || len(rec.snapshot()) != 1 {
		t.Fatalf("bad json err %v stdout %q stderr %q hits %d", err, stdout, stderr, len(rec.snapshot()))
	}
	stdout, stderr, err = runHook(t, "{", "hook", "gemini", "--verbose", "--url", ts.URL+"/api/jobs")
	if err != nil || stdout != "{}\n" || !strings.Contains(stderr, "statuspan:") {
		t.Fatalf("verbose err %v stdout %q stderr %q", err, stdout, stderr)
	}
	stdout, stderr, err = runHook(t, handled, "hook", "gemini", "--url", "http://127.0.0.1:1/api/jobs")
	if err != nil || stdout != "{}\n" {
		t.Fatalf("down board err %v stdout %q stderr %q", err, stdout, stderr)
	}
}

func TestGeminiGrokOverrideWins(t *testing.T) {
	useGeminiFailMarkerDir(t)
	clearProxy(t)
	ts, rec := newPostServer(t)
	t.Setenv("GROK_HOOK_EVENT", "UserPromptSubmit")
	payload := `{"session_id":"abcdef1234567890","cwd":"/work/My Project","hook_event_name":"UserPromptSubmit","prompt":"please fix the login bug"}`
	stdout, stderr, err := runHook(t, payload, "hook", "gemini", "--url", ts.URL+"/api/jobs")
	if err != nil || stdout != "" || stderr != "" {
		t.Fatalf("err %v stdout %q stderr %q", err, stdout, stderr)
	}
	hits := rec.snapshot()
	if len(hits) != 1 || hits[0].body["agent"] != "Grok" || hits[0].body["job_id"] != grokJob || hits[0].body["state"] != "running" {
		t.Fatalf("hits %+v", hits)
	}

	rec.reset()
	beforeAgent := `{"session_id":"abcdef1234567890","cwd":"/work/My Project","hook_event_name":"BeforeAgent","prompt":"please fix the login bug"}`
	stdout, stderr, err = runHook(t, beforeAgent, "hook", "gemini", "--url", ts.URL+"/api/jobs")
	if err != nil || stdout != "" || stderr != "" || len(rec.snapshot()) != 0 {
		t.Fatalf("before agent err %v stdout %q stderr %q hits %d", err, stdout, stderr, len(rec.snapshot()))
	}
}

func TestGeminiHooksInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	original := []byte(`{
  "theme": "dark",
  "model": "gemini-2.5-pro",
  "hooksConfig": {"enabled": true},
  "security": {"auth": {"selectedType": "oauth-personal"}},
  "hooks": {
    "SessionStart": [
      {"hooks": [{"type": "command", "command": "echo keep-me"}]}
    ],
    "BeforeTool": [
      {
        "matcher": "run_shell_command",
        "hooks": [
          {"type": "command", "command": "/usr/local/bin/statuspan hook gemini --addr 1", "name": "statuspan"},
          {"type": "command", "command": "/tmp/statuspan hook claude --addr 1"}
        ]
      }
    ]
  }
}
`)
	if err := os.WriteFile(path, original, 0o640); err != nil {
		t.Fatal(err)
	}
	bin := "/tmp/statuspan"
	wantCmd := bin + " hook gemini --url http://127.0.0.1:9/api/jobs"
	args := []string{
		"hooks", "install", "gemini",
		"--settings", path,
		"--bin", bin,
		"--url", "http://127.0.0.1:9/api/jobs",
	}

	stdout, stderr, err := runCLI(t, append(append([]string{}, args...), "--dry-run")...)
	if err != nil {
		t.Fatal(err)
	}
	if stderr != "" || !strings.HasSuffix(stdout, "\n") || !json.Valid([]byte(stdout)) || !strings.Contains(stdout, wantCmd) || !strings.Contains(stdout, `"theme": "dark"`) || !strings.Contains(stdout, `"hooksConfig"`) {
		t.Fatalf("dry-run stdout %s stderr %q", stdout, stderr)
	}
	if got := mustRead(t, path); !bytes.Equal(got, original) {
		t.Fatal("dry-run wrote the file")
	}
	if n := len(backupFiles(t, path)); n != 0 {
		t.Fatalf("dry-run backups %d", n)
	}

	stdout, stderr, err = runCLI(t, args...)
	if err != nil || stdout != "" {
		t.Fatalf("install err %v stdout %q", err, stdout)
	}
	if strings.Contains(stderr, claudeRestartHint) || !strings.Contains(stderr, "Backup ") {
		t.Fatalf("stderr %q", stderr)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	installed := mustRead(t, path)
	assertGeminiHandlers(t, installed, wantCmd)
	assertGeminiSettingsKept(t, installed)
	if strings.Contains(string(installed), "hook gemini --addr 1") || strings.Contains(string(installed), "--token ") {
		t.Fatalf("old or token command remained\n%s", installed)
	}

	if _, _, err := runCLI(t, args...); err != nil {
		t.Fatal(err)
	}
	again := mustRead(t, path)
	assertGeminiHandlers(t, again, wantCmd)
	if strings.Count(string(again), wantCmd) != 6 {
		t.Fatalf("duplicate install\n%s", again)
	}

	replaced := bin + " hook gemini --url http://127.0.0.1:8/api/jobs"
	if _, _, err := runCLI(t, "hooks", "install", "gemini", "--settings", path, "--bin", bin, "--url", "http://127.0.0.1:8/api/jobs"); err != nil {
		t.Fatal(err)
	}
	swapped := string(mustRead(t, path))
	if strings.Contains(swapped, wantCmd) || strings.Count(swapped, replaced) != 6 {
		t.Fatalf("replace\n%s", swapped)
	}
	assertGeminiSettingsKept(t, []byte(swapped))

	stdout, stderr, err = runCLI(t, "hooks", "uninstall", "gemini", "--settings", path, "--bin", bin)
	if err != nil || stdout != "" {
		t.Fatalf("uninstall err %v stdout %q", err, stdout)
	}
	removed := string(mustRead(t, path))
	if strings.Contains(removed, "hook gemini") || !strings.Contains(removed, "echo keep-me") || !strings.Contains(removed, "hook claude --addr 1") {
		t.Fatalf("uninstall\n%s", removed)
	}
	assertGeminiSettingsKept(t, []byte(removed))
	if _, ok := decodeSettings(t, []byte(removed))["hooks"]; !ok {
		t.Fatal("hooks key dropped while a foreign hook remained")
	}

	only := filepath.Join(dir, "only.json")
	onlyOriginal := []byte(`{"theme":"dark","hooksConfig":{"enabled":true}}`)
	if err := os.WriteFile(only, onlyOriginal, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "hooks", "install", "gemini", "--settings", only, "--bin", bin); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "hooks", "uninstall", "gemini", "--settings", only, "--bin", bin); err != nil {
		t.Fatal(err)
	}
	doc := decodeSettings(t, mustRead(t, only))
	if _, ok := doc["hooks"]; ok {
		t.Fatalf("hooks key remained %+v", doc)
	}
	if doc["theme"] != "dark" {
		t.Fatalf("theme %+v", doc["theme"])
	}
	cfg, _ := doc["hooksConfig"].(map[string]any)
	if cfg["enabled"] != true {
		t.Fatalf("hooksConfig %+v", doc["hooksConfig"])
	}

	fresh := filepath.Join(dir, "fresh.json")
	if _, _, err := runCLI(t, "hooks", "install", "gemini", "--settings", fresh, "--bin", bin); err != nil {
		t.Fatal(err)
	}
	freshText := string(mustRead(t, fresh))
	if strings.Contains(freshText, "hooksConfig") {
		t.Fatalf("fresh file gained hooksConfig\n%s", freshText)
	}
	assertGeminiHandlers(t, []byte(freshText), bin+" hook gemini")

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "hooks", "install", "gemini", "--settings", bad, "--bin", bin); err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("bad json err %v", err)
	}
	if string(mustRead(t, bad)) != "{nope" {
		t.Fatal("invalid json changed")
	}

	arrayPath := filepath.Join(dir, "array.json")
	if err := os.WriteFile(arrayPath, []byte(`{"hooks":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "hooks", "install", "gemini", "--settings", arrayPath, "--bin", bin); err == nil {
		t.Fatal("hooks array accepted")
	}
	if got := mustRead(t, arrayPath); string(got) != `{"hooks":[]}` {
		t.Fatalf("array file changed %s", got)
	}

	tokenPath := filepath.Join(dir, "token.json")
	if _, _, err := runCLI(t, "hooks", "install", "gemini", "--settings", tokenPath, "--token", "sekret", "--bin", bin); err == nil || !strings.Contains(err.Error(), "--token-file") {
		t.Fatalf("token err %v", err)
	}
	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Fatal("token flag wrote")
	}

	missing := filepath.Join(dir, "missing.json")
	stdout, stderr, err = runCLI(t, "hooks", "uninstall", "gemini", "--settings", missing, "--bin", bin)
	if err != nil || stdout != "" || !strings.Contains(stderr, "No settings file") {
		t.Fatalf("missing err %v out %q errOut %q", err, stdout, stderr)
	}

	stdout, stderr, err = runCLI(t, "hooks", "print", "gemini", "--bin", bin, "--settings", filepath.Join(dir, "print.json"), "--home", dir, "--dir", dir)
	if err != nil || stderr != "" || !strings.HasSuffix(stdout, "\n") || !json.Valid([]byte(stdout)) || !strings.Contains(stdout, bin+" hook gemini") {
		t.Fatalf("print err %v stderr %q stdout %s", err, stderr, stdout)
	}
	if _, err := os.Stat(filepath.Join(dir, "print.json")); !os.IsNotExist(err) {
		t.Fatal("print wrote settings")
	}
	if _, err := os.Stat(filepath.Join(dir, ".gemini")); !os.IsNotExist(err) {
		t.Fatal("print created .gemini")
	}

	home := t.TempDir()
	if _, _, err := runCLI(t, "hooks", "install", "gemini", "--home", home, "--bin", bin); err != nil {
		t.Fatal(err)
	}
	homeFile := filepath.Join(home, ".gemini", "settings.json")
	if _, err := os.Stat(homeFile); err != nil {
		t.Fatal(err)
	}
	assertGeminiHandlers(t, mustRead(t, homeFile), bin+" hook gemini")

	project := t.TempDir()
	stdout, stderr, err = runCLI(t, "hooks", "install", "gemini", "--project", "--dir", project, "--bin", bin, "--dry-run")
	if err != nil || stderr != "" || !strings.Contains(stdout, bin+" hook gemini") || !strings.Contains(stdout, `"timeout": 3000`) {
		t.Fatalf("project dry-run err %v stdout %s stderr %q", err, stdout, stderr)
	}
	projectFile := filepath.Join(project, ".gemini", "settings.json")
	if _, err := os.Stat(projectFile); !os.IsNotExist(err) {
		t.Fatal("dry-run wrote project settings")
	}
	stdout, stderr, err = runCLI(t, "hooks", "install", "gemini", "--project", "--dir", project, "--bin", bin)
	if err != nil || stdout != "" {
		t.Fatalf("project err %v stdout %q", err, stdout)
	}
	if _, err := os.Stat(projectFile); err != nil {
		t.Fatal(err)
	}
	assertGeminiHandlers(t, mustRead(t, projectFile), bin+" hook gemini")
	if _, err := os.Stat(filepath.Join(home, ".gemini", "settings.json")); err != nil {
		t.Fatal("project install removed the home file")
	}
}

func assertGeminiSettingsKept(t *testing.T, data []byte) {
	t.Helper()
	doc := decodeSettings(t, data)
	if doc["theme"] != "dark" || doc["model"] != "gemini-2.5-pro" {
		t.Fatalf("settings %+v", doc)
	}
	cfg, _ := doc["hooksConfig"].(map[string]any)
	if cfg["enabled"] != true {
		t.Fatalf("hooksConfig %+v", doc["hooksConfig"])
	}
	security, _ := doc["security"].(map[string]any)
	auth, _ := security["auth"].(map[string]any)
	if auth["selectedType"] != "oauth-personal" {
		t.Fatalf("security %+v", doc["security"])
	}
}

func assertGeminiHandlers(t *testing.T, data []byte, wantCmd string) {
	t.Helper()
	doc := decodeSettings(t, data)
	text := string(data)
	if strings.Count(text, wantCmd) != 6 {
		t.Fatalf("command count %d\n%s", strings.Count(text, wantCmd), text)
	}
	if strings.Contains(text, `"async"`) {
		t.Fatalf("async set\n%s", text)
	}
	hooks, _ := doc["hooks"].(map[string]any)
	check := func(event, matcher string, timeout float64) {
		t.Helper()
		groups, _ := hooks[event].([]any)
		var found map[string]any
		for _, g := range groups {
			gm, _ := g.(map[string]any)
			hs, _ := gm["hooks"].([]any)
			for _, h := range hs {
				hm, _ := h.(map[string]any)
				cmd, _ := hm["command"].(string)
				if !strings.Contains(cmd, wantCmd) {
					continue
				}
				found = gm
				if hm["type"] != "command" {
					t.Fatalf("%s type %+v", event, hm["type"])
				}
				if hm["name"] != "statuspan" {
					t.Fatalf("%s name %+v", event, hm["name"])
				}
				if hm["description"] != geminiHandlerDescription {
					t.Fatalf("%s description %+v", event, hm["description"])
				}
				if _, hasAsync := hm["async"]; hasAsync {
					t.Fatalf("%s has async", event)
				}
				if hm["timeout"] != timeout {
					t.Fatalf("%s timeout %v", event, hm["timeout"])
				}
			}
		}
		if found == nil {
			t.Fatalf("%s handler missing\n%s", event, text)
		}
		if matcher == "" {
			if _, ok := found["matcher"]; ok {
				t.Fatalf("%s matcher %+v", event, found["matcher"])
			}
		} else if found["matcher"] != matcher {
			t.Fatalf("%s matcher %+v", event, found["matcher"])
		}
	}
	check("SessionStart", "", 3000)
	check("BeforeAgent", "", 3000)
	check("BeforeTool", ".*", 3000)
	check("Notification", "", 3000)
	check("AfterAgent", "", 5000)
	check("SessionEnd", "", 3000)
	for _, event := range []string{"AfterTool", "BeforeModel", "AfterModel", "BeforeToolSelection", "PreCompress"} {
		if _, ok := hooks[event]; ok {
			t.Fatalf("registered %s", event)
		}
	}
}

func useGeminiFailMarkerDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := geminiFailMarkerDir
	geminiFailMarkerDir = dir
	t.Cleanup(func() { geminiFailMarkerDir = prev })
	return dir
}
