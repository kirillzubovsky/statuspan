package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const cursorJob = "cursor-my-project-abcdef12"

func TestCursorHookEvents(t *testing.T) {
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
		job      string
	}{
		{
			name: "session start agent",
			in: with(cursorEv("sessionStart"), func(in *claudeHookInput) {
				in.ComposerMode = "agent"
			}),
			state:    "queued",
			hasTitle: true,
			title:    "My Project: Cursor session",
			note:     "Session started (agent)",
		},
		{
			name: "session start edit",
			in: with(cursorEv("sessionStart"), func(in *claudeHookInput) {
				in.ComposerMode = "edit"
			}),
			state:    "queued",
			hasTitle: true,
			title:    "My Project: Cursor session",
			note:     "Session started (edit)",
		},
		{
			name: "session start ask skipped",
			in: with(cursorEv("sessionStart"), func(in *claudeHookInput) {
				in.ComposerMode = "ask"
			}),
			skip: true,
		},
		{
			name: "session start ASK skipped",
			in: with(cursorEv("SessionStart"), func(in *claudeHookInput) {
				in.ComposerMode = "ASK"
			}),
			skip: true,
		},
		{
			name:     "session start pascal case",
			in:       with(cursorEv("SessionStart"), func(in *claudeHookInput) { in.ComposerMode = "agent" }),
			state:    "queued",
			hasTitle: true,
			title:    "My Project: Cursor session",
			note:     "Session started (agent)",
		},
		{
			name: "prompt",
			in: with(cursorEv("beforeSubmitPrompt"), func(in *claudeHookInput) {
				in.Prompt = "please fix the login bug"
			}),
			state:    "running",
			hasTitle: true,
			title:    "please fix the login bug",
			note:     "Working on the prompt",
		},
		{
			name: "short follow-up omits title",
			in: with(cursorEv("beforeSubmitPrompt"), func(in *claudeHookInput) {
				in.Prompt = "go ahead"
			}),
			state: "running",
			note:  "Working on the prompt",
		},
		{
			name: "slash command omits title",
			in: with(cursorEv("beforeSubmitPrompt"), func(in *claudeHookInput) {
				in.Prompt = "/review the board"
			}),
			state: "running",
			note:  "Working on the prompt",
		},
		{
			name: "shell",
			in: with(cursorEv("postToolUse"), func(in *claudeHookInput) {
				in.ToolName = "Shell"
				in.ToolInput = json.RawMessage("{\"command\":\"go test ./...\\nsecond\",\"working_directory\":\"/work/My Project\"}")
			}),
			state: "running",
			note:  "Run: go test ./...",
		},
		{
			name: "read",
			in: with(cursorEv("postToolUse"), func(in *claudeHookInput) {
				in.ToolName = "Read"
				in.ToolInput = json.RawMessage(`{"file_path":"/work/My Project/cmd/main.go"}`)
			}),
			state: "running",
			note:  "Read cmd/main.go",
		},
		{
			name: "write path key",
			in: with(cursorEv("postToolUse"), func(in *claudeHookInput) {
				in.ToolName = "Write"
				in.ToolInput = json.RawMessage(`{"path":"/work/My Project/README.md"}`)
			}),
			state: "running",
			note:  "Write README.md",
		},
		{
			name: "delete target file",
			in: with(cursorEv("postToolUse"), func(in *claudeHookInput) {
				in.ToolName = "Delete"
				in.ToolInput = json.RawMessage(`{"target_file":"/work/My Project/old.go"}`)
			}),
			state: "running",
			note:  "Delete old.go",
		},
		{
			name: "grep",
			in: with(cursorEv("postToolUse"), func(in *claudeHookInput) {
				in.ToolName = "Grep"
				in.ToolInput = json.RawMessage(`{"pattern":"hook cursor"}`)
			}),
			state: "running",
			note:  "Grep hook cursor",
		},
		{
			name: "task",
			in: with(cursorEv("postToolUse"), func(in *claudeHookInput) {
				in.ToolName = "Task"
				in.ToolInput = json.RawMessage(`{"description":"scan the hooks"}`)
			}),
			state: "running",
			note:  "Subagent: scan the hooks",
		},
		{
			name: "mcp",
			in: with(cursorEv("postToolUse"), func(in *claudeHookInput) {
				in.ToolName = "MCP:read_file"
			}),
			state: "running",
			note:  "MCP read_file",
		},
		{
			name: "shell secret",
			in: with(cursorEv("postToolUse"), func(in *claudeHookInput) {
				in.ToolName = "Shell"
				in.ToolInput = json.RawMessage(`{"command":"curl token=supersecret"}`)
			}),
			state: "running",
			note:  "Run: curl token=[redacted]",
		},
		{
			name: "after file edit",
			in: with(cursorEv("afterFileEdit"), func(in *claudeHookInput) {
				in.FilePath = "/work/My Project/cmd/main.go"
			}),
			state: "running",
			note:  "Edit cmd/main.go",
		},
		{
			name: "stop completed",
			in: with(cursorEv("stop"), func(in *claudeHookInput) {
				in.Status = "completed"
			}),
			state:    "done",
			note:     "Turn finished",
			progress: &hundred,
		},
		{
			name: "stop aborted",
			in: with(cursorEv("stop"), func(in *claudeHookInput) {
				in.Status = "aborted"
			}),
			state: "done",
			note:  "Turn stopped",
		},
		{
			name: "stop error",
			in: with(cursorEv("stop"), func(in *claudeHookInput) {
				in.Status = "error"
			}),
			state: "failed",
			note:  "Turn failed",
		},
		{
			name: "stop unknown status ignored",
			in: with(cursorEv("stop"), func(in *claudeHookInput) {
				in.Status = "other"
			}),
			skip: true,
		},
		{
			name: "session end completed",
			in: with(cursorEv("sessionEnd"), func(in *claudeHookInput) {
				in.Reason = "completed"
			}),
			state: "done",
			note:  "Session ended (completed)",
		},
		{
			name: "session end window close",
			in: with(cursorEv("sessionEnd"), func(in *claudeHookInput) {
				in.Reason = "window_close"
			}),
			state: "done",
			note:  "Session ended (window_close)",
		},
		{
			name: "session end user close",
			in: with(cursorEv("sessionEnd"), func(in *claudeHookInput) {
				in.Reason = "user_close"
			}),
			state: "done",
			note:  "Session ended (user_close)",
		},
		{
			name: "session end aborted",
			in: with(cursorEv("sessionEnd"), func(in *claudeHookInput) {
				in.Reason = "aborted"
			}),
			state: "done",
			note:  "Session ended (aborted)",
		},
		{
			name: "session end error",
			in: with(cursorEv("sessionEnd"), func(in *claudeHookInput) {
				in.Reason = "error"
				in.ErrorMessage = "token=supersecret boom"
			}),
			state: "failed",
			note:  "Session ended with an error: token=[redacted] boom",
		},
		{
			name: "session end error truncated",
			in: with(cursorEv("sessionEnd"), func(in *claudeHookInput) {
				in.Reason = "error"
				in.ErrorMessage = flexString(strings.Repeat("e", 90))
			}),
			state: "failed",
			note:  "Session ended with an error: " + strings.Repeat("e", 80),
		},
		{
			name: "permission hook ignored",
			in:   cursorEv("preToolUse"),
			skip: true,
		},
		{
			name: "before shell ignored",
			in:   cursorEv("beforeShellExecution"),
			skip: true,
		},
		{
			name: "empty session ignored",
			in: with(cursorEv("stop"), func(in *claudeHookInput) {
				in.ConversationID = ""
				in.SessionID = ""
				in.Status = "completed"
			}),
			skip: true,
		},
		{
			name: "session id fallback",
			in: with(cursorEv("sessionEnd"), func(in *claudeHookInput) {
				in.ConversationID = ""
				in.SessionID = testSession
				in.Reason = "completed"
			}),
			state: "done",
			note:  "Session ended (completed)",
		},
		{
			name: "conversation id wins",
			in: with(cursorEv("sessionEnd"), func(in *claudeHookInput) {
				in.ConversationID = "zzzzzzzz99999999"
				in.SessionID = testSession
				in.Reason = "completed"
			}),
			state: "done",
			note:  "Session ended (completed)",
			job:   "cursor-my-project-zzzzzzzz",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useCursorFailMarkerDir(t)
			got, ok := cursorUpdate(tc.in, "Cursor")
			if tc.skip {
				if ok {
					t.Fatalf("posted %+v", got)
				}
				return
			}
			if !ok {
				t.Fatal("no post")
			}
			wantJob := tc.job
			if wantJob == "" {
				wantJob = cursorJob
			}
			if got.Agent != "Cursor" || got.JobID != wantJob || got.State != tc.state {
				t.Fatalf("identity %+v", got)
			}
			if got.TTL == nil || *got.TTL != hookTTL {
				t.Fatalf("ttl %+v", got.TTL)
			}
			if got.Seq == nil || *got.Seq == 0 {
				t.Fatalf("seq %+v", got.Seq)
			}
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

	useCursorFailMarkerDir(t)
	got, ok := cursorUpdate(with(cursorEv("stop"), func(in *claudeHookInput) { in.Status = "completed" }), "")
	if !ok || got.Agent != "Cursor" || got.JobID != cursorJob || noteOf(got) != "Turn finished" {
		t.Fatalf("default agent %+v ok %v", got, ok)
	}
}

func TestCursorSessionStartSeqBackdated(t *testing.T) {
	useCursorFailMarkerDir(t)
	start := with(cursorEv("sessionStart"), func(in *claudeHookInput) { in.ComposerMode = "agent" })
	prompt := with(cursorEv("beforeSubmitPrompt"), func(in *claudeHookInput) { in.Prompt = "please fix the login bug" })

	before := nextSeq()
	got, ok := cursorUpdate(start, "Cursor")
	after := nextSeq()
	if !ok || got.Seq == nil {
		t.Fatal("session start posted nothing")
	}
	assigned := *got.Seq + sessionStartSeqBackdate
	if assigned <= before || assigned >= after {
		t.Fatalf("session seq %d is not nextSeq()-2000 between %d and %d", *got.Seq, before, after)
	}
	promptUp, ok := cursorUpdate(prompt, "Cursor")
	if !ok || promptUp.Seq == nil {
		t.Fatal("prompt posted nothing")
	}
	late, ok := cursorUpdate(start, "Cursor")
	if !ok || late.Seq == nil || *late.Seq >= *promptUp.Seq {
		t.Fatalf("late sessionStart seq %+v prompt %+v", late.Seq, promptUp.Seq)
	}
}

func TestCursorFailMarker(t *testing.T) {
	dir := useCursorFailMarkerDir(t)
	end := with(cursorEv("sessionEnd"), func(in *claudeHookInput) { in.Reason = "completed" })
	ask := with(cursorEv("sessionStart"), func(in *claudeHookInput) { in.ComposerMode = "ask" })

	writeFailMarker(dir, testSession)
	if _, ok := cursorUpdate(ask, "Cursor"); ok {
		t.Fatal("ask mode posted")
	}
	if !failMarkerExists(dir, testSession) {
		t.Fatal("ask mode cleared the fail marker")
	}

	got, ok := cursorUpdate(with(cursorEv("stop"), func(in *claudeHookInput) { in.Status = "error" }), "Cursor")
	if !ok || got.State != "failed" || noteOf(got) != "Turn failed" || !failMarkerExists(dir, testSession) {
		t.Fatalf("stop error ok %v state %s note %q marker %v", ok, got.State, noteOf(got), failMarkerExists(dir, testSession))
	}

	got, ok = cursorUpdate(end, "Cursor")
	if !ok || got.State != "failed" || noteOf(got) != "Session ended after an API error (completed)" {
		t.Fatalf("session end ok %v state %s note %q", ok, got.State, noteOf(got))
	}
	if failMarkerExists(dir, testSession) {
		t.Fatal("marker remained after session end")
	}

	writeFailMarker(dir, testSession)
	got, ok = cursorUpdate(with(cursorEv("sessionEnd"), func(in *claudeHookInput) {
		in.Reason = "error"
		in.ErrorMessage = "disk full"
	}), "Cursor")
	if !ok || got.State != "failed" || noteOf(got) != "Session ended with an error: disk full" {
		t.Fatalf("error end ok %v state %s note %q", ok, got.State, noteOf(got))
	}
	if failMarkerExists(dir, testSession) {
		t.Fatal("error end left the marker")
	}

	writeFailMarker(dir, testSession)
	if _, ok := cursorUpdate(with(cursorEv("beforeSubmitPrompt"), func(in *claudeHookInput) { in.Prompt = "go ahead" }), "Cursor"); !ok {
		t.Fatal("prompt posted nothing")
	}
	if failMarkerExists(dir, testSession) {
		t.Fatal("prompt left the fail marker")
	}

	writeFailMarker(dir, testSession)
	got, ok = cursorUpdate(with(cursorEv("stop"), func(in *claudeHookInput) { in.Status = "completed" }), "Cursor")
	if !ok || got.State != "done" || got.Progress == nil || *got.Progress != 100 || noteOf(got) != "Turn finished" {
		t.Fatalf("completed ok %v %+v", ok, got)
	}
	if failMarkerExists(dir, testSession) {
		t.Fatal("completed left the marker")
	}

	writeFailMarker(dir, testSession)
	got, ok = cursorUpdate(with(cursorEv("stop"), func(in *claudeHookInput) { in.Status = "aborted" }), "Cursor")
	if !ok || got.State != "done" || got.Progress != nil || noteOf(got) != "Turn stopped" {
		t.Fatalf("aborted ok %v %+v", ok, got)
	}
	if failMarkerExists(dir, testSession) {
		t.Fatal("aborted left the marker")
	}
}

func TestCursorCwdFallbacks(t *testing.T) {
	useCursorFailMarkerDir(t)
	t.Setenv("CURSOR_PROJECT_DIR", "")

	withCwd := cursorEv("sessionEnd")
	withCwd.Reason = "completed"
	withCwd.WorkspaceRoots = json.RawMessage(`["/work/Other App"]`)
	got, ok := cursorUpdate(withCwd, "Cursor")
	if !ok || got.JobID != cursorJob || got.Title != nil {
		t.Fatalf("tool cwd ok %v %+v", ok, got)
	}

	fromRoot := claudeHookInput{
		ConversationID: testSession,
		HookEventName:  "sessionEnd",
		Reason:         "completed",
		WorkspaceRoots: json.RawMessage(`["","/work/Other App"]`),
	}
	got, ok = cursorUpdate(fromRoot, "Cursor")
	if !ok || got.JobID != "cursor-other-app-abcdef12" {
		t.Fatalf("workspace root ok %v job %s", ok, got.JobID)
	}

	t.Setenv("CURSOR_PROJECT_DIR", "/work/Env Project")
	fromEnv := claudeHookInput{
		SessionID:     testSession,
		HookEventName: "afterFileEdit",
		FilePath:      "/work/Env Project/cmd/main.go",
	}
	got, ok = cursorUpdate(fromEnv, "Cursor")
	if !ok || got.JobID != "cursor-env-project-abcdef12" || noteOf(got) != "Edit cmd/main.go" {
		t.Fatalf("env ok %v job %s note %q", ok, got.JobID, noteOf(got))
	}

	t.Setenv("CURSOR_PROJECT_DIR", "")
	proc := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(proc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	fromProc := claudeHookInput{
		ConversationID: testSession,
		HookEventName:  "sessionStart",
		ComposerMode:   "agent",
	}
	got, ok = cursorUpdate(fromProc, "Cursor")
	wantID, idOK := hookJobID("cursor", proc, testSession)
	if !ok || !idOK || got.JobID != wantID || got.Title == nil || *got.Title != sessionTitle(proc, "Cursor") {
		t.Fatalf("process cwd ok %v job %s title %+v want %s", ok, got.JobID, got.Title, wantID)
	}
}

func TestCursorHookStdout(t *testing.T) {
	useCursorFailMarkerDir(t)
	clearProxy(t)
	ts, rec := newPostServer(t)
	prompt := `{"conversation_id":"abcdef1234567890","cwd":"/work/My Project","hook_event_name":"beforeSubmitPrompt","prompt":"please fix the login bug"}`
	stdout, stderr, err := runHook(t, prompt, "hook", "cursor", "--url", ts.URL+"/api/jobs")
	if err != nil || stdout != "{\"continue\":true}\n" || stderr != "" {
		t.Fatalf("prompt err %v stdout %q stderr %q", err, stdout, stderr)
	}
	hits := rec.snapshot()
	if len(hits) != 1 || hits[0].body["agent"] != "Cursor" || hits[0].body["job_id"] != cursorJob || hits[0].body["state"] != "running" {
		t.Fatalf("hits %+v", hits)
	}

	others := []string{
		`{"conversation_id":"abcdef1234567890","cwd":"/work/My Project","hook_event_name":"sessionStart","composer_mode":"agent"}`,
		`{"conversation_id":"abcdef1234567890","cwd":"/work/My Project","hook_event_name":"postToolUse","tool_name":"Shell","tool_input":{"command":"true"}}`,
		`{"conversation_id":"abcdef1234567890","cwd":"/work/My Project","hook_event_name":"afterFileEdit","file_path":"/work/My Project/a.go"}`,
		`{"conversation_id":"abcdef1234567890","cwd":"/work/My Project","hook_event_name":"stop","status":"completed"}`,
		`{"conversation_id":"abcdef1234567890","cwd":"/work/My Project","hook_event_name":"sessionEnd","reason":"completed"}`,
		`{"conversation_id":"abcdef1234567890","cwd":"/work/My Project","hook_event_name":"preToolUse","tool_name":"Shell"}`,
		`{"conversation_id":"abcdef1234567890","cwd":"/work/My Project","hookEventName":"stop","status":"aborted"}`,
		`{}`,
	}
	for _, payload := range others {
		stdout, stderr, err = runHook(t, payload, "hook", "cursor", "--url", ts.URL+"/api/jobs")
		if err != nil || stdout != "{}\n" || stderr != "" {
			t.Fatalf("other err %v stdout %q stderr %q payload %s", err, stdout, stderr, payload)
		}
		if strings.Contains(stdout, "followup_message") || strings.Contains(stdout, "continue") {
			t.Fatalf("stdout %q", stdout)
		}
	}

	for _, payload := range []string{"", " ", "{", "null", "[]", "\"x\""} {
		before := len(rec.snapshot())
		stdout, stderr, err = runHook(t, payload, "hook", "cursor", "--url", ts.URL+"/api/jobs")
		if err != nil || stdout != "{\"continue\":true}\n" || stderr != "" || len(rec.snapshot()) != before {
			t.Fatalf("bad err %v stdout %q stderr %q hits %d payload %q", err, stdout, stderr, len(rec.snapshot()), payload)
		}
	}
	stdout, stderr, err = runHook(t, "{", "hook", "cursor", "--verbose", "--url", ts.URL+"/api/jobs")
	if err != nil || stdout != "{\"continue\":true}\n" || !strings.Contains(stderr, "statuspan:") {
		t.Fatalf("verbose err %v stdout %q stderr %q", err, stdout, stderr)
	}

	ts.Close()
	stdout, stderr, err = runHook(t, prompt, "hook", "cursor", "--url", ts.URL+"/api/jobs")
	if err != nil || stdout != "{\"continue\":true}\n" {
		t.Fatalf("down board err %v stdout %q stderr %q", err, stdout, stderr)
	}
	stdout, stderr, err = runHook(t, others[4], "hook", "cursor", "--url", ts.URL+"/api/jobs")
	if err != nil || stdout != "{}\n" {
		t.Fatalf("down stop err %v stdout %q stderr %q", err, stdout, stderr)
	}
}

func TestCursorGrokOverrideWins(t *testing.T) {
	useCursorFailMarkerDir(t)
	clearProxy(t)
	ts, rec := newPostServer(t)
	t.Setenv("GROK_HOOK_EVENT", "beforeSubmitPrompt")
	payload := `{"conversation_id":"abcdef1234567890","cwd":"/work/My Project","hook_event_name":"beforeSubmitPrompt","prompt":"please fix the login bug"}`
	stdout, stderr, err := runHook(t, payload, "hook", "cursor", "--url", ts.URL+"/api/jobs")
	if err != nil || stdout != "" || stderr != "" || len(rec.snapshot()) != 0 {
		t.Fatalf("err %v stdout %q stderr %q hits %d", err, stdout, stderr, len(rec.snapshot()))
	}
}

func TestCursorHooksInstall(t *testing.T) {
	guardRealCursorHooks(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	original := []byte(`{
  "description": "keep me",
  "hooks": {
    "sessionStart": [
      {"command": "echo keep-me"}
    ],
    "afterFileEdit": [
      {"command": "echo keep-edit"}
    ],
    "preToolUse": [
      {"command": "echo keep-permission", "matcher": "Shell"}
    ],
    "beforeShellExecution": [
      {"command": "/tmp/statuspan hook cursor --addr 1", "timeout": 3}
    ]
  }
}
`)
	if err := os.WriteFile(path, original, 0o640); err != nil {
		t.Fatal(err)
	}
	bin := "/tmp/statuspan"
	wantCmd := bin + " hook cursor --url http://127.0.0.1:9/api/jobs"
	args := []string{
		"hooks", "install", "cursor",
		"--settings", path,
		"--bin", bin,
		"--url", "http://127.0.0.1:9/api/jobs",
	}

	stdout, stderr, err := runCLI(t, append(append([]string{}, args...), "--dry-run")...)
	if err != nil {
		t.Fatal(err)
	}
	if stderr != "" || !strings.HasSuffix(stdout, "\n") || !json.Valid([]byte(stdout)) || !strings.Contains(stdout, wantCmd) || !strings.Contains(stdout, `"description": "keep me"`) || !strings.Contains(stdout, `"version": 1`) {
		t.Fatalf("dry-run stdout %s stderr %q", stdout, stderr)
	}
	if strings.Contains(stderr, cursorReloadHint) {
		t.Fatalf("dry-run hint %q", stderr)
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
	if !strings.Contains(stderr, cursorReloadHint) || !strings.Contains(stderr, "Backup ") || strings.Contains(stderr, claudeRestartHint) {
		t.Fatalf("stderr %q", stderr)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	assertCursorHandlers(t, mustRead(t, path), wantCmd, true)

	if _, _, err := runCLI(t, args...); err != nil {
		t.Fatal(err)
	}
	assertCursorHandlers(t, mustRead(t, path), wantCmd, true)
	if n := len(backupFiles(t, path)); n != 2 {
		t.Fatalf("backups %d", n)
	}

	replaced := bin + " hook cursor --url http://127.0.0.1:8/api/jobs"
	if _, _, err := runCLI(t, "hooks", "install", "cursor", "--settings", path, "--bin", bin, "--url", "http://127.0.0.1:8/api/jobs"); err != nil {
		t.Fatal(err)
	}
	again := string(mustRead(t, path))
	if strings.Contains(again, wantCmd) || strings.Count(again, replaced) != 6 {
		t.Fatalf("replace\n%s", again)
	}
	assertNoCursorPermissionInstall(t, []byte(again), replaced)

	stdout, stderr, err = runCLI(t, "hooks", "uninstall", "cursor", "--settings", path, "--bin", bin)
	if err != nil || stdout != "" {
		t.Fatalf("uninstall err %v stdout %q", err, stdout)
	}
	if strings.Contains(stderr, cursorReloadHint) || strings.Contains(stderr, claudeRestartHint) {
		t.Fatalf("uninstall stderr %q", stderr)
	}
	removed := mustRead(t, path)
	text := string(removed)
	if strings.Contains(text, "hook cursor") || !strings.Contains(text, "echo keep-me") || !strings.Contains(text, "echo keep-edit") || !strings.Contains(text, "echo keep-permission") || !strings.Contains(text, "keep me") {
		t.Fatalf("uninstall\n%s", text)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("uninstall removed the file")
	}
	doc := decodeSettings(t, removed)
	hooks, _ := doc["hooks"].(map[string]any)
	if _, ok := hooks["beforeShellExecution"]; ok {
		t.Fatalf("empty permission event kept\n%s", text)
	}
	if _, ok := hooks["stop"]; ok {
		t.Fatalf("empty stop event kept\n%s", text)
	}
	if doc["version"] != float64(1) || doc["description"] != "keep me" {
		t.Fatalf("kept fields %+v", doc)
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "hooks", "install", "cursor", "--settings", bad, "--bin", bin); err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("bad json err %v", err)
	}
	if string(mustRead(t, bad)) != "{nope" {
		t.Fatal("invalid json changed")
	}

	wrong := filepath.Join(dir, "v2.json")
	v2 := []byte(`{"version":2,"description":"stay","hooks":{}}`)
	if err := os.WriteFile(wrong, v2, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "hooks", "install", "cursor", "--settings", wrong, "--bin", bin); err == nil || !strings.Contains(err.Error(), "not 1") {
		t.Fatalf("version err %v", err)
	}
	if got := mustRead(t, wrong); !bytes.Equal(got, v2) {
		t.Fatalf("version file changed %s", got)
	}
	if _, _, err := runCLI(t, "hooks", "install", "cursor", "--settings", wrong, "--bin", bin, "--dry-run"); err == nil || !strings.Contains(err.Error(), "not 1") {
		t.Fatalf("dry-run version err %v", err)
	}

	fresh := filepath.Join(dir, "token.json")
	if _, _, err := runCLI(t, "hooks", "install", "cursor", "--settings", fresh, "--token", "sekret", "--bin", bin); err == nil || !strings.Contains(err.Error(), "--token-file") {
		t.Fatalf("token err %v", err)
	}
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Fatal("token flag wrote")
	}

	missing := filepath.Join(dir, "missing.json")
	stdout, stderr, err = runCLI(t, "hooks", "uninstall", "cursor", "--settings", missing, "--bin", bin)
	if err != nil || stdout != "" || !strings.Contains(stderr, "No hooks file") {
		t.Fatalf("missing err %v out %q errOut %q", err, stdout, stderr)
	}

	stdout, stderr, err = runCLI(t, "hooks", "print", "cursor", "--bin", bin, "--settings", filepath.Join(dir, "print.json"))
	if err != nil || stderr != "" || !strings.HasSuffix(stdout, "\n") || !json.Valid([]byte(stdout)) || !strings.Contains(stdout, bin+" hook cursor") || strings.Contains(stdout, "matcher") {
		t.Fatalf("print err %v stderr %q stdout %s", err, stderr, stdout)
	}
	assertCursorHandlers(t, []byte(stdout), bin+" hook cursor", false)
	if _, err := os.Stat(filepath.Join(dir, "print.json")); !os.IsNotExist(err) {
		t.Fatal("print wrote settings")
	}

	home := t.TempDir()
	if _, _, err := runCLI(t, "hooks", "install", "cursor", "--home", home, "--bin", bin); err != nil {
		t.Fatal(err)
	}
	homeFile := filepath.Join(home, ".cursor", "hooks.json")
	if _, err := os.Stat(homeFile); err != nil {
		t.Fatal(err)
	}
	assertNoCursorPermissionInstall(t, mustRead(t, homeFile), bin+" hook cursor")

	project := t.TempDir()
	stdout, stderr, err = runCLI(t, "hooks", "install", "cursor", "--project", "--dir", project, "--bin", bin, "--dry-run")
	if err != nil || stderr != "" || !strings.Contains(stdout, bin+" hook cursor") || !strings.Contains(stdout, `"version": 1`) {
		t.Fatalf("project dry-run err %v stdout %s stderr %q", err, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(project, ".cursor", "hooks.json")); !os.IsNotExist(err) {
		t.Fatal("dry-run wrote project hooks")
	}
	stdout, stderr, err = runCLI(t, "hooks", "install", "cursor", "--project", "--dir", project, "--bin", bin)
	if err != nil || stdout != "" {
		t.Fatalf("project err %v stdout %q", err, stdout)
	}
	if !strings.Contains(stderr, cursorReloadHint) {
		t.Fatalf("project stderr %q", stderr)
	}
	projectFile := filepath.Join(project, ".cursor", "hooks.json")
	if _, err := os.Stat(projectFile); err != nil {
		t.Fatal(err)
	}
	assertCursorHandlers(t, mustRead(t, projectFile), bin+" hook cursor", false)
	assertNoCursorPermissionInstall(t, mustRead(t, projectFile), bin+" hook cursor")
}

func assertCursorHandlers(t *testing.T, data []byte, wantCmd string, merged bool) {
	t.Helper()
	doc := decodeSettings(t, data)
	if doc["version"] != float64(1) {
		t.Fatalf("version %+v", doc["version"])
	}
	if merged {
		if doc["description"] != "keep me" {
			t.Fatalf("description %+v", doc["description"])
		}
		text := string(data)
		if !strings.Contains(text, "echo keep-me") || !strings.Contains(text, "echo keep-edit") || !strings.Contains(text, "echo keep-permission") {
			t.Fatalf("foreign hooks\n%s", text)
		}
		if strings.Contains(text, "hook cursor --addr 1") {
			t.Fatalf("old cursor hook remained\n%s", text)
		}
	}
	if strings.Count(string(data), wantCmd) != 6 {
		t.Fatalf("command count %d\n%s", strings.Count(string(data), wantCmd), data)
	}
	hooks, _ := doc["hooks"].(map[string]any)
	check := func(event string, timeout float64) {
		t.Helper()
		items, _ := hooks[event].([]any)
		var found map[string]any
		for _, item := range items {
			hm, _ := item.(map[string]any)
			if hm["command"] != wantCmd {
				continue
			}
			found = hm
			if _, ok := hm["matcher"]; ok {
				t.Fatalf("%s matcher %+v", event, hm["matcher"])
			}
			if _, ok := hm["type"]; ok {
				t.Fatalf("%s type %+v", event, hm["type"])
			}
			if hm["timeout"] != timeout {
				t.Fatalf("%s timeout %v", event, hm["timeout"])
			}
		}
		if found == nil {
			t.Fatalf("%s handler missing", event)
		}
	}
	check("sessionStart", 3)
	check("beforeSubmitPrompt", 3)
	check("postToolUse", 3)
	check("afterFileEdit", 3)
	check("stop", 5)
	check("sessionEnd", 3)
	assertNoCursorPermissionInstall(t, data, wantCmd)
}

func assertNoCursorPermissionInstall(t *testing.T, data []byte, wantCmd string) {
	t.Helper()
	doc := decodeSettings(t, data)
	hooks, _ := doc["hooks"].(map[string]any)
	for _, event := range []string{
		"preToolUse",
		"beforeShellExecution",
		"beforeMCPExecution",
		"beforeReadFile",
		"subagentStart",
		"beforeTabFileRead",
	} {
		raw, ok := hooks[event]
		if !ok {
			continue
		}
		text, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(text), "hook cursor") || strings.Contains(string(text), wantCmd) {
			t.Fatalf("wrote %s\n%s", event, data)
		}
	}
	if strings.Count(string(data), wantCmd) != 0 {
		for _, event := range []string{"sessionStart", "beforeSubmitPrompt", "postToolUse", "afterFileEdit", "stop", "sessionEnd"} {
			if _, ok := hooks[event]; !ok && strings.Contains(string(data), `"`+event+`"`) {
				t.Fatalf("event %s missing\n%s", event, data)
			}
		}
	}
}

// guardRealCursorHooks restores ~/.cursor/hooks.json if a test writes it.
// Install tests pass a temp dir instead.
func guardRealCursorHooks(t *testing.T) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	path := filepath.Join(home, ".cursor", "hooks.json")
	data, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	snapshot := append([]byte(nil), data...)
	t.Cleanup(func() {
		now, readErr := os.ReadFile(path)
		same := (!exists && os.IsNotExist(readErr)) || (exists && readErr == nil && bytes.Equal(snapshot, now))
		if same {
			return
		}
		if !exists {
			_ = os.Remove(path)
		} else if writeErr := os.WriteFile(path, snapshot, 0o600); writeErr != nil {
			t.Errorf("restore %s: %v", path, writeErr)
		}
		t.Errorf("restored Cursor hooks file %s", path)
	})
}

func useCursorFailMarkerDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := cursorFailMarkerDir
	cursorFailMarkerDir = dir
	t.Cleanup(func() { cursorFailMarkerDir = prev })
	return dir
}

func cursorEv(event string) claudeHookInput {
	return claudeHookInput{
		ConversationID: testSession,
		Cwd:            testCwd,
		HookEventName:  flexString(event),
	}
}
