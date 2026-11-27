package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const codexJob = "codex-my-project-abcdef12"

func TestCodexHookEvents(t *testing.T) {
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
			title:    "My Project: Codex session",
			note:     "Session started (startup)",
		},
		{
			name:     "session start resume",
			in:       with(ev("SessionStart"), func(in *claudeHookInput) { in.Source = "resume" }),
			state:    "queued",
			hasTitle: true,
			title:    "My Project: Codex session",
			note:     "Session started (resume)",
		},
		{
			name:     "session start clear",
			in:       with(ev("SessionStart"), func(in *claudeHookInput) { in.Source = "clear" }),
			state:    "queued",
			hasTitle: true,
			title:    "My Project: Codex session",
			note:     "Session started (clear)",
		},
		{
			name:     "session start compact",
			in:       with(ev("SessionStart"), func(in *claudeHookInput) { in.Source = "compact" }),
			state:    "queued",
			hasTitle: true,
			title:    "My Project: Codex session",
			note:     "Session started (compact)",
		},
		{
			name:     "session start fork",
			in:       with(ev("SessionStart"), func(in *claudeHookInput) { in.Source = "fork" }),
			state:    "queued",
			hasTitle: true,
			title:    "My Project: Codex session",
			note:     "Session started (fork)",
		},
		{
			name:     "user prompt",
			in:       with(ev("UserPromptSubmit"), func(in *claudeHookInput) { in.Prompt = "please fix the login bug" }),
			state:    "running",
			hasTitle: true,
			title:    "please fix the login bug",
			note:     "Working on the prompt",
		},
		{
			name:  "short follow-up omits title",
			in:    with(ev("UserPromptSubmit"), func(in *claudeHookInput) { in.Prompt = "go ahead" }),
			state: "running",
			note:  "Working on the prompt",
		},
		{
			name:  "slash command omits title",
			in:    with(ev("UserPromptSubmit"), func(in *claudeHookInput) { in.Prompt = "/review the board" }),
			state: "running",
			note:  "Working on the prompt",
		},
		{
			name: "bash is run",
			in: with(ev("PreToolUse"), func(in *claudeHookInput) {
				in.ToolName = "Bash"
				in.ToolInput = json.RawMessage(`{"command":"go test ./...\nsecond"}`)
			}),
			state: "running",
			note:  "Run: go test ./...",
		},
		{
			name: "permission request blocks",
			in: with(ev("PermissionRequest"), func(in *claudeHookInput) {
				in.ToolName = "Bash"
				in.ToolInput = json.RawMessage(`{"command":"rm -rf /tmp/x"}`)
			}),
			state: "blocked",
			note:  "Needs your approval: Run: rm -rf /tmp/x",
		},
		{
			name:     "stop finishes the turn",
			in:       with(ev("Stop"), func(in *claudeHookInput) { in.LastAssistantMessage = "hello\nsecond line" }),
			state:    "done",
			note:     "Turn finished: hello",
			progress: &hundred,
		},
		{
			name:     "stop without a message",
			in:       ev("Stop"),
			state:    "done",
			note:     "Turn finished",
			progress: &hundred,
		},
		{
			name: "stop ignores background tasks",
			in: with(ev("Stop"), func(in *claudeHookInput) {
				in.BackgroundTasks = json.RawMessage(`[{}]`)
				in.LastAssistantMessage = "done now"
			}),
			state:    "done",
			note:     "Turn finished: done now",
			progress: &hundred,
		},
		{
			name: "stop hook active ignored",
			in:   with(ev("Stop"), func(in *claudeHookInput) { in.StopHookActive = true }),
			skip: true,
		},
		{
			name:     "stop with agent id still finishes",
			in:       with(ev("Stop"), func(in *claudeHookInput) { in.AgentID = "agent_1" }),
			state:    "done",
			note:     "Turn finished",
			progress: &hundred,
		},
		{
			name:  "interrupt",
			in:    ev("Interrupt"),
			state: "done",
			note:  "Turn interrupted",
		},
		{
			name:  "session end",
			in:    with(ev("SessionEnd"), func(in *claudeHookInput) { in.Reason = "other" }),
			state: "done",
			note:  "Session ended (other)",
		},
		{
			name: "subagent start ignored",
			in:   ev("SubagentStart"),
			skip: true,
		},
		{
			name: "subagent stop ignored",
			in:   ev("SubagentStop"),
			skip: true,
		},
		{
			name: "post tool use ignored",
			in:   ev("PostToolUse"),
			skip: true,
		},
		{
			name: "pre compact ignored",
			in:   ev("PreCompact"),
			skip: true,
		},
		{
			name: "post compact ignored",
			in:   ev("PostCompact"),
			skip: true,
		},
		{
			name: "empty session ignored",
			in:   with(ev("Stop"), func(in *claudeHookInput) { in.SessionID = "" }),
			skip: true,
		},
	}
	var prev int64
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useCodexFailMarkerDir(t)
			got, ok := codexUpdate(tc.in, "Codex")
			if tc.skip {
				if ok {
					t.Fatalf("posted %+v", got)
				}
				return
			}
			if !ok {
				t.Fatal("no post")
			}
			if got.Agent != "Codex" || got.JobID != codexJob || got.State != tc.state {
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

	got, ok := codexUpdate(ev("Interrupt"), "")
	if !ok || got.Agent != "Codex" || got.JobID != codexJob || noteOf(got) != "Turn interrupted" {
		t.Fatalf("default agent %+v ok %v", got, ok)
	}
}

func TestCodexSessionStartSeqBackdated(t *testing.T) {
	useCodexFailMarkerDir(t)
	start := with(ev("SessionStart"), func(in *claudeHookInput) { in.Source = "startup" })
	prompt := with(ev("UserPromptSubmit"), func(in *claudeHookInput) { in.Prompt = "please fix the login bug" })

	before := nextSeq()
	got, ok := codexUpdate(start, "Codex")
	after := nextSeq()
	if !ok || got.Seq == nil {
		t.Fatal("session start posted nothing")
	}
	assigned := *got.Seq + sessionStartSeqBackdate
	if assigned <= before || assigned >= after {
		t.Fatalf("session seq %d is not nextSeq()-2000 between %d and %d", *got.Seq, before, after)
	}
	promptUp, ok := codexUpdate(prompt, "Codex")
	if !ok || promptUp.Seq == nil {
		t.Fatal("prompt posted nothing")
	}
	late, ok := codexUpdate(start, "Codex")
	if !ok || late.Seq == nil || *late.Seq >= *promptUp.Seq {
		t.Fatalf("late SessionStart seq %+v prompt %+v", late.Seq, promptUp.Seq)
	}
}

func TestCodexNullAssistantMessage(t *testing.T) {
	useCodexFailMarkerDir(t)
	in, err := parseClaudeHook([]byte(`{"session_id":"abcdef1234567890","cwd":"/work/My Project","hook_event_name":"Stop","last_assistant_message":null}`))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := codexUpdate(in, "Codex")
	if !ok || got.State != "done" || noteOf(got) != "Turn finished" || got.Progress == nil || *got.Progress != 100 {
		t.Fatalf("ok %v %+v", ok, got)
	}
}

func TestCodexApplyPatch(t *testing.T) {
	useCodexFailMarkerDir(t)
	cases := []struct {
		name  string
		input string
		note  string
	}{
		{
			name:  "single",
			input: `{"command":"*** Begin Patch\n*** Update File: /work/My Project/cmd/main.go\n@@\n-old\n+brand new line\n*** End Patch"}`,
			note:  "Edit cmd/main.go",
		},
		{
			name:  "multiple",
			input: `{"command":"*** Add File: /work/My Project/a.go\r\n*** Update File: b.go\r\n*** Delete File: /elsewhere/c.go\r\n"}`,
			note:  "Edit a.go (+2 more)",
		},
		{
			name:  "spaced path",
			input: `{"command":"*** Update File: /work/My Project/my file.go"}`,
			note:  "Edit my file.go",
		},
		{
			name:  "none",
			input: `{"command":"*** Begin Patch\n@@\n-old\n+new\n*** End Patch"}`,
			note:  "Edit files",
		},
		{
			name:  "empty command",
			input: `{"command":""}`,
			note:  "Edit files",
		},
		{
			name:  "missing command",
			input: `{}`,
			note:  "Edit files",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := with(ev("PreToolUse"), func(in *claudeHookInput) {
				in.ToolName = "apply_patch"
				in.ToolInput = json.RawMessage(tc.input)
			})
			got, ok := codexUpdate(in, "Codex")
			if !ok || got.State != "running" || got.Title != nil || noteOf(got) != tc.note {
				t.Fatalf("ok %v note %q title %+v", ok, noteOf(got), got.Title)
			}
			if strings.Contains(noteOf(got), "brand new") || strings.Contains(noteOf(got), "Begin Patch") || strings.HasPrefix(noteOf(got), "Run:") {
				t.Fatalf("patch leaked into %q", noteOf(got))
			}
		})
	}

	in := with(ev("PermissionRequest"), func(in *claudeHookInput) {
		in.ToolName = "apply_patch"
		in.ToolInput = json.RawMessage(`{"command":"*** Delete File: /work/My Project/main.go"}`)
	})
	got, ok := codexUpdate(in, "Codex")
	if !ok || got.State != "blocked" || noteOf(got) != "Needs your approval: Edit main.go" {
		t.Fatalf("approval ok %v note %q", ok, noteOf(got))
	}
}

func TestCodexToolNotes(t *testing.T) {
	useCodexFailMarkerDir(t)
	cases := []struct {
		name  string
		tool  string
		input string
		note  string
	}{
		{name: "bash secret", tool: "Bash", input: `{"command":"curl token=supersecret"}`, note: "Run: curl token=[redacted]"},
		{name: "bash truncated", tool: "Bash", input: `{"command":"` + strings.Repeat("x", 100) + `"}`, note: "Run: " + strings.Repeat("x", 80)},
		{name: "mcp", tool: "mcp__fs__read_file", input: `{"path":"/work/My Project/main.go"}`, note: "MCP fs: read_file"},
		{name: "read file", tool: "read_file", input: `{"path":"/work/My Project/main.go"}`, note: "Read main.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := with(ev("PreToolUse"), func(in *claudeHookInput) {
				in.ToolName = flexString(tc.tool)
				in.ToolInput = json.RawMessage(tc.input)
			})
			got, ok := codexUpdate(in, "Codex")
			if !ok || noteOf(got) != tc.note {
				t.Fatalf("ok %v note %q", ok, noteOf(got))
			}
		})
	}
}

func TestCodexFailMarker(t *testing.T) {
	dir := useCodexFailMarkerDir(t)
	end := with(ev("SessionEnd"), func(in *claudeHookInput) { in.Reason = "other" })

	writeFailMarker(dir, testSession)
	got, ok := codexUpdate(end, "Codex")
	if !ok || got.State != "failed" || noteOf(got) != "Session ended after an API error (other)" {
		t.Fatalf("session end ok %v state %s note %q", ok, got.State, noteOf(got))
	}
	if failMarkerExists(dir, testSession) {
		t.Fatal("marker remained after session end")
	}

	writeFailMarker(dir, testSession)
	if _, ok := codexUpdate(with(ev("UserPromptSubmit"), func(in *claudeHookInput) { in.Prompt = "go ahead" }), "Codex"); !ok {
		t.Fatal("prompt posted nothing")
	}
	if failMarkerExists(dir, testSession) {
		t.Fatal("prompt left the fail marker")
	}
	got, ok = codexUpdate(end, "Codex")
	if !ok || got.State != "done" || noteOf(got) != "Session ended (other)" {
		t.Fatalf("cleared session end ok %v state %s note %q", ok, got.State, noteOf(got))
	}

	writeFailMarker(dir, testSession)
	got, ok = codexUpdate(ev("Stop"), "Codex")
	if !ok || got.State != "done" || noteOf(got) != "Turn finished" {
		t.Fatalf("stop ok %v state %s note %q", ok, got.State, noteOf(got))
	}
	if failMarkerExists(dir, testSession) {
		t.Fatal("stop left the fail marker")
	}
}

func TestCodexHookStdout(t *testing.T) {
	useCodexFailMarkerDir(t)
	clearProxy(t)
	ts, rec := newPostServer(t)
	handled := `{"session_id":"abcdef1234567890","cwd":"/work/My Project","hook_event_name":"UserPromptSubmit","prompt":"please fix the login bug"}`
	stdout, stderr, err := runHook(t, handled, "hook", "codex", "--url", ts.URL+"/api/jobs")
	if err != nil || stdout != "{}\n" || stderr != "" {
		t.Fatalf("handled err %v stdout %q stderr %q", err, stdout, stderr)
	}
	hits := rec.snapshot()
	if len(hits) != 1 || hits[0].body["agent"] != "Codex" || hits[0].body["job_id"] != codexJob || hits[0].body["state"] != "running" {
		t.Fatalf("hits %+v", hits)
	}

	ignored := []string{
		`{"session_id":"abcdef1234567890","cwd":"/work/My Project","hook_event_name":"SubagentStop"}`,
		`{"session_id":"abcdef1234567890","cwd":"/work/My Project","hook_event_name":"SubagentStart"}`,
		`{"session_id":"abcdef1234567890","cwd":"/work/My Project","hook_event_name":"PostToolUse"}`,
		`{"session_id":"abcdef1234567890","cwd":"/work/My Project","hook_event_name":"PreCompact"}`,
		`{"session_id":"abcdef1234567890","cwd":"/work/My Project","hook_event_name":"PostCompact"}`,
		`{"session_id":"abcdef1234567890","cwd":"/work/My Project","hook_event_name":"Stop","stop_hook_active":true,"last_assistant_message":"hidden"}`,
	}
	for _, payload := range ignored {
		before := len(rec.snapshot())
		stdout, stderr, err = runHook(t, payload, "hook", "codex", "--url", ts.URL+"/api/jobs")
		if err != nil || stdout != "{}\n" || stderr != "" || len(rec.snapshot()) != before {
			t.Fatalf("ignored err %v stdout %q stderr %q payload %s", err, stdout, stderr, payload)
		}
	}

	stdout, stderr, err = runHook(t, "", "hook", "codex", "--url", ts.URL+"/api/jobs")
	if err != nil || stdout != "{}\n" || stderr != "" || len(rec.snapshot()) != 1 {
		t.Fatalf("empty err %v stdout %q stderr %q hits %d", err, stdout, stderr, len(rec.snapshot()))
	}
	stdout, stderr, err = runHook(t, "{", "hook", "codex", "--url", ts.URL+"/api/jobs")
	if err != nil || stdout != "{}\n" || stderr != "" || len(rec.snapshot()) != 1 {
		t.Fatalf("bad json err %v stdout %q stderr %q hits %d", err, stdout, stderr, len(rec.snapshot()))
	}
	stdout, stderr, err = runHook(t, "{", "hook", "codex", "--verbose", "--url", ts.URL+"/api/jobs")
	if err != nil || stdout != "{}\n" || !strings.Contains(stderr, "statuspan:") {
		t.Fatalf("verbose err %v stdout %q stderr %q", err, stdout, stderr)
	}
}

func TestCodexHooksInstall(t *testing.T) {
	guardRealCodexHooks(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)

	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	original := []byte(`{
  "description": "keep me",
  "version": 1,
  "hooks": {
    "SessionStart": [
      {"hooks": [{"type": "command", "command": "echo keep-me"}]}
    ],
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {"type": "command", "command": "/usr/local/bin/statuspan hook codex --addr 1"},
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
	wantCmd := bin + " hook codex --url http://127.0.0.1:9/api/jobs"
	args := []string{
		"hooks", "install", "codex",
		"--settings", path,
		"--bin", bin,
		"--url", "http://127.0.0.1:9/api/jobs",
	}

	stdout, stderr, err := runCLI(t, append(append([]string{}, args...), "--dry-run")...)
	if err != nil {
		t.Fatal(err)
	}
	if stderr != "" || !strings.HasSuffix(stdout, "\n") || !json.Valid([]byte(stdout)) || !strings.Contains(stdout, wantCmd) || !strings.Contains(stdout, `"description": "keep me"`) {
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
	if !strings.Contains(stderr, codexTrustHint) || strings.Contains(stderr, codexProjectHint) || strings.Contains(stderr, claudeRestartHint) || !strings.Contains(stderr, "Backup ") {
		t.Fatalf("stderr %q", stderr)
	}
	if _, err := os.Stat(filepath.Join(codexHome, "hooks.json")); !os.IsNotExist(err) {
		t.Fatal("CODEX_HOME written during --settings install")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	assertCodexHandlers(t, mustRead(t, path), wantCmd)

	if _, _, err := runCLI(t, args...); err != nil {
		t.Fatal(err)
	}
	assertCodexHandlers(t, mustRead(t, path), wantCmd)

	replaced := bin + " hook codex --url http://127.0.0.1:8/api/jobs"
	if _, _, err := runCLI(t, "hooks", "install", "codex", "--settings", path, "--bin", bin, "--url", "http://127.0.0.1:8/api/jobs"); err != nil {
		t.Fatal(err)
	}
	again := string(mustRead(t, path))
	if strings.Contains(again, wantCmd) || strings.Count(again, replaced) != 7 {
		t.Fatalf("replace\n%s", again)
	}

	stdout, stderr, err = runCLI(t, "hooks", "uninstall", "codex", "--settings", path, "--bin", bin)
	if err != nil || stdout != "" {
		t.Fatalf("uninstall err %v stdout %q", err, stdout)
	}
	if strings.Contains(stderr, codexTrustHint) || strings.Contains(stderr, claudeRestartHint) {
		t.Fatalf("uninstall stderr %q", stderr)
	}
	removed := string(mustRead(t, path))
	if strings.Contains(removed, "hook codex") || !strings.Contains(removed, "echo keep-me") || !strings.Contains(removed, "hook claude --addr 1") || !strings.Contains(removed, "keep me") {
		t.Fatalf("uninstall\n%s", removed)
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "hooks", "install", "codex", "--settings", bad, "--bin", bin); err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("bad json err %v", err)
	}
	if string(mustRead(t, bad)) != "{nope" {
		t.Fatal("invalid json changed")
	}

	fresh := filepath.Join(dir, "token.json")
	if _, _, err := runCLI(t, "hooks", "install", "codex", "--settings", fresh, "--token", "sekret", "--bin", bin); err == nil || !strings.Contains(err.Error(), "--token-file") {
		t.Fatalf("token err %v", err)
	}
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Fatal("token flag wrote")
	}

	missing := filepath.Join(dir, "missing.json")
	stdout, stderr, err = runCLI(t, "hooks", "uninstall", "codex", "--settings", missing, "--bin", bin)
	if err != nil || stdout != "" || !strings.Contains(stderr, "No settings file") {
		t.Fatalf("missing err %v out %q errOut %q", err, stdout, stderr)
	}

	stdout, stderr, err = runCLI(t, "hooks", "print", "codex", "--bin", bin, "--settings", filepath.Join(dir, "print.json"))
	if err != nil || stderr != "" || !strings.HasSuffix(stdout, "\n") || !json.Valid([]byte(stdout)) || !strings.Contains(stdout, bin+" hook codex") {
		t.Fatalf("print err %v stderr %q stdout %s", err, stderr, stdout)
	}
	if _, err := os.Stat(filepath.Join(dir, "print.json")); !os.IsNotExist(err) {
		t.Fatal("print wrote settings")
	}
}

func TestCodexHomeAndProject(t *testing.T) {
	guardRealCodexHooks(t)
	bin := "/tmp/statuspan"
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)

	stdout, stderr, err := runCLI(t, "hooks", "install", "codex", "--bin", bin)
	if err != nil || stdout != "" {
		t.Fatalf("codex home err %v stdout %q", err, stdout)
	}
	if !strings.Contains(stderr, codexTrustHint) || strings.Contains(stderr, codexProjectHint) {
		t.Fatalf("stderr %q", stderr)
	}
	if _, err := os.Stat(filepath.Join(codexHome, "hooks.json")); err != nil {
		t.Fatal(err)
	}

	homeOverride := t.TempDir()
	other := t.TempDir()
	t.Setenv("CODEX_HOME", other)
	if _, _, err := runCLI(t, "hooks", "install", "codex", "--home", homeOverride, "--bin", bin); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(homeOverride, ".codex", "hooks.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(other, "hooks.json")); !os.IsNotExist(err) {
		t.Fatal("--home still wrote CODEX_HOME")
	}

	project := t.TempDir()
	stdout, stderr, err = runCLI(t, "hooks", "install", "codex", "--project", "--dir", project, "--bin", bin, "--dry-run")
	if err != nil || stderr != "" || !strings.Contains(stdout, bin+" hook codex") {
		t.Fatalf("project dry-run err %v stdout %s stderr %q", err, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(project, ".codex", "hooks.json")); !os.IsNotExist(err) {
		t.Fatal("dry-run wrote project hooks")
	}
	stdout, stderr, err = runCLI(t, "hooks", "install", "codex", "--project", "--dir", project, "--bin", bin)
	if err != nil || stdout != "" {
		t.Fatalf("project err %v stdout %q", err, stdout)
	}
	if !strings.Contains(stderr, codexTrustHint) || !strings.Contains(stderr, codexProjectHint) {
		t.Fatalf("project stderr %q", stderr)
	}
	if _, err := os.Stat(filepath.Join(project, ".codex", "hooks.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(other, "hooks.json")); !os.IsNotExist(err) {
		t.Fatal("project install wrote CODEX_HOME")
	}
}

func assertCodexHandlers(t *testing.T, data []byte, wantCmd string) {
	t.Helper()
	doc := decodeSettings(t, data)
	if doc["description"] != "keep me" || doc["version"] != float64(1) {
		t.Fatalf("kept fields %+v", doc)
	}
	text := string(data)
	if strings.Count(text, "echo keep-me") != 1 || !strings.Contains(text, "/tmp/statuspan hook claude --addr 1") {
		t.Fatalf("foreign hooks\n%s", text)
	}
	if strings.Contains(text, "hook codex --addr 1") {
		t.Fatalf("old codex hook remained\n%s", text)
	}
	if strings.Count(text, wantCmd) != 7 {
		t.Fatalf("command count %d\n%s", strings.Count(text, wantCmd), text)
	}
	hooks, _ := doc["hooks"].(map[string]any)
	check := func(event, matcher string, async bool, timeout float64) {
		t.Helper()
		groups, _ := hooks[event].([]any)
		var found map[string]any
		for _, g := range groups {
			gm, _ := g.(map[string]any)
			hs, _ := gm["hooks"].([]any)
			for _, h := range hs {
				hm, _ := h.(map[string]any)
				if hm["command"] != wantCmd {
					continue
				}
				found = gm
				if hm["type"] != "command" {
					t.Fatalf("%s type %+v", event, hm["type"])
				}
				_, hasAsync := hm["async"]
				if async {
					if hm["async"] != true {
						t.Fatalf("%s async %+v", event, hm["async"])
					}
				} else if hasAsync {
					t.Fatalf("%s has async", event)
				}
				gotTimeout, hasTimeout := hm["timeout"]
				if timeout == 0 {
					if hasTimeout {
						t.Fatalf("%s timeout %v", event, gotTimeout)
					}
				} else if gotTimeout != timeout {
					t.Fatalf("%s timeout %v", event, gotTimeout)
				}
			}
		}
		if found == nil {
			t.Fatalf("%s handler missing", event)
		}
		if matcher == "" {
			if _, ok := found["matcher"]; ok {
				t.Fatalf("%s matcher %+v", event, found["matcher"])
			}
		} else if found["matcher"] != matcher {
			t.Fatalf("%s matcher %+v", event, found["matcher"])
		}
	}
	check("SessionStart", "", false, 3)
	check("UserPromptSubmit", "", true, 0)
	check("PreToolUse", "*", true, 0)
	check("PermissionRequest", "*", true, 0)
	check("Stop", "", false, 5)
	check("Interrupt", "", false, 2)
	check("SessionEnd", "", false, 3)
}

// guardRealCodexHooks restores ~/.codex/hooks.json and $CODEX_HOME/hooks.json
// if a test writes them. Install tests pass a temp dir instead.
func guardRealCodexHooks(t *testing.T) {
	t.Helper()
	var paths []string
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".codex", "hooks.json"))
	}
	if codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME")); codexHome != "" {
		paths = append(paths, filepath.Join(codexHome, "hooks.json"))
	}
	type snap struct {
		path   string
		data   []byte
		exists bool
	}
	snaps := make([]snap, 0, len(paths))
	seen := map[string]bool{}
	for _, path := range paths {
		if seen[path] {
			continue
		}
		seen[path] = true
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		snaps = append(snaps, snap{path: path, data: append([]byte(nil), data...), exists: err == nil})
	}
	t.Cleanup(func() {
		for _, s := range snaps {
			data, err := os.ReadFile(s.path)
			same := (!s.exists && os.IsNotExist(err)) || (s.exists && err == nil && bytes.Equal(s.data, data))
			if same {
				continue
			}
			if !s.exists {
				_ = os.Remove(s.path)
			} else if writeErr := os.WriteFile(s.path, s.data, 0o600); writeErr != nil {
				t.Errorf("restore %s: %v", s.path, writeErr)
			}
			t.Errorf("restored Codex hooks file %s", s.path)
		}
	})
}

func useCodexFailMarkerDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := codexFailMarkerDir
	codexFailMarkerDir = dir
	t.Cleanup(func() { codexFailMarkerDir = prev })
	return dir
}
