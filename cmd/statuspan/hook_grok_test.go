package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const grokJob = "grok-my-project-abcdef12"

func TestGrokHookEvents(t *testing.T) {
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
			in:       with(ev("SessionStart"), func(in *claudeHookInput) { in.Source = "new" }),
			state:    "queued",
			hasTitle: true,
			title:    "My Project: Grok session",
			note:     "Session started (new)",
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
			name: "pre tool use",
			in: with(ev("PreToolUse"), func(in *claudeHookInput) {
				in.ToolName = "read_file"
				in.ToolInput = []byte(`{"target_file":"/work/My Project/main.go"}`)
			}),
			state: "running",
			note:  "Read main.go",
		},
		{name: "post tool use ignored", in: ev("PostToolUse"), skip: true},
		{name: "post tool use failure ignored", in: ev("PostToolUseFailure"), skip: true},
		{name: "permission denied ignored", in: ev("PermissionDenied"), skip: true},
		{
			name: "permission prompt blocks",
			in: with(ev("Notification"), func(in *claudeHookInput) {
				in.NotificationType = "permission_prompt"
				in.Message = "allow the edit"
			}),
			state: "blocked",
			note:  "Needs your input: allow the edit",
		},
		{
			name: "idle notification ignored",
			in:   with(ev("Notification"), func(in *claudeHookInput) { in.NotificationType = "idle_prompt" }),
			skip: true,
		},
		{
			name: "task complete ignored",
			in:   with(ev("Notification"), func(in *claudeHookInput) { in.NotificationType = "task_complete" }),
			skip: true,
		},
		{
			name:     "stop finishes the turn",
			in:       with(ev("Stop"), func(in *claudeHookInput) { in.Reason = "end_turn"; in.LastAssistantMessage = "hello" }),
			state:    "done",
			note:     "Turn finished: hello",
			progress: &hundred,
		},
		{
			name: "shutdown stop ignored",
			in:   with(ev("Stop"), func(in *claudeHookInput) { in.Reason = "shutdown" }),
			skip: true,
		},
		{
			name: "shutdown stop ignored with no message",
			in: with(ev("Stop"), func(in *claudeHookInput) {
				in.Reason = "shutdown"
				in.LastAssistantMessage = ""
			}),
			skip: true,
		},
		{
			name:  "stop failure",
			in:    with(ev("StopFailure"), func(in *claudeHookInput) { in.Error = "overloaded" }),
			state: "failed",
			note:  "API error: overloaded",
		},
		{
			name:  "stop cancelled",
			in:    with(ev("StopCancelled"), func(in *claudeHookInput) { in.Reason = "user_interrupt" }),
			state: "done",
			note:  "Turn stopped (user_interrupt)",
		},
		{
			name:  "stop cancelled permission",
			in:    with(ev("StopCancelled"), func(in *claudeHookInput) { in.Reason = "permission_rejected" }),
			state: "done",
			note:  "Turn stopped (permission_rejected)",
		},
		{
			name:  "session end",
			in:    with(ev("SessionEnd"), func(in *claudeHookInput) { in.Reason = "shutdown" }),
			state: "done",
			note:  "Session ended (shutdown)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useGrokFailMarkerDir(t)
			got, ok := grokUpdate(tc.in, "Grok")
			if tc.skip {
				if ok {
					t.Fatalf("posted %+v", got)
				}
				return
			}
			if !ok {
				t.Fatal("no post")
			}
			if got.Agent != "Grok" || got.JobID != grokJob || got.State != tc.state {
				t.Fatalf("identity %+v", got)
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
}

func TestGrokShutdownStopDoesNotClearFailure(t *testing.T) {
	dir := useGrokFailMarkerDir(t)
	fail := with(ev("StopFailure"), func(in *claudeHookInput) { in.Error = "overloaded" })
	if _, ok := grokUpdate(fail, "Grok"); !ok {
		t.Fatal("stop failure posted nothing")
	}
	marker := filepath.Join(dir, slugify(testSession, 0)+".failed")
	if _, err := os.Stat(marker); err != nil {
		t.Fatal(err)
	}
	shutdown := with(ev("Stop"), func(in *claudeHookInput) { in.Reason = "shutdown" })
	if _, ok := grokUpdate(shutdown, "Grok"); ok {
		t.Fatal("shutdown stop posted")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("shutdown stop cleared the fail marker")
	}
}

func TestGrokStopCancelledClearsFailMarker(t *testing.T) {
	dir := useGrokFailMarkerDir(t)
	fail := with(ev("StopFailure"), func(in *claudeHookInput) { in.Error = "overloaded" })
	if _, ok := grokUpdate(fail, "Grok"); !ok {
		t.Fatal("stop failure posted nothing")
	}
	marker := filepath.Join(dir, slugify(testSession, 0)+".failed")
	got, ok := grokUpdate(with(ev("StopCancelled"), func(in *claudeHookInput) { in.Reason = "max_turns" }), "Grok")
	if !ok || got.State != "done" || noteOf(got) != "Turn stopped (max_turns)" || got.Progress != nil {
		t.Fatalf("ok %v state %s note %q progress %+v", ok, got.State, noteOf(got), got.Progress)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker after cancel: %v", err)
	}
}

func TestClaudeShutdownStopStillPosts(t *testing.T) {
	useFailMarkerDir(t)
	in := with(ev("Stop"), func(in *claudeHookInput) { in.Reason = "shutdown" })
	got, ok := claudeUpdate(in, "Claude Code")
	if !ok || got.State != "done" || got.JobID != testJob || noteOf(got) != "Turn finished" {
		t.Fatalf("ok %v %+v", ok, got)
	}
}

func TestGrokCamelCasePayload(t *testing.T) {
	useGrokFailMarkerDir(t)
	body := []byte(`{"sessionId":"abcdef1234567890","workspaceRoot":"/work/My Project","hookEventName":"user_prompt_submit","prompt":"please fix the login bug"}`)
	in, err := parseClaudeHook(body)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := grokUpdate(in, "")
	if !ok || got.Agent != "Grok" || got.JobID != grokJob || got.State != "running" {
		t.Fatalf("ok %v %+v", ok, got)
	}
	if got.Title == nil || *got.Title != "please fix the login bug" || noteOf(got) != "Working on the prompt" {
		t.Fatalf("text title %#v note %q", got.Title, noteOf(got))
	}

	toolBody := []byte(`{"sessionId":"abcdef1234567890","workspaceRoot":"/work/My Project","hookEventName":"pre_tool_use","toolName":"search_replace","toolInput":{"file_path":"/work/My Project/main.go"}}`)
	in, err = parseClaudeHook(toolBody)
	if err != nil {
		t.Fatal(err)
	}
	got, ok = grokUpdate(in, "Grok")
	if !ok || got.State != "running" || noteOf(got) != "Edit main.go" {
		t.Fatalf("ok %v note %q", ok, noteOf(got))
	}

	stopBody := []byte(`{"sessionId":"abcdef1234567890","cwd":"/work/My Project","hookEventName":"stop","reason":"shutdown"}`)
	in, err = parseClaudeHook(stopBody)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := grokUpdate(in, "Grok"); ok {
		t.Fatal("camelCase shutdown stop posted")
	}
}

func TestGrokToolNotes(t *testing.T) {
	cwd := "/work/app"
	cases := []struct {
		tool  string
		input string
		note  string
	}{
		{tool: "read_file", input: `{"target_file":"/work/app/main.go"}`, note: "Read main.go"},
		{tool: "search_replace", input: `{"file_path":"/work/app/src/main.go"}`, note: "Edit src/main.go"},
		{tool: "edit_file", input: `{"path":"/work/app/a.go"}`, note: "Edit a.go"},
		{tool: "replace", input: `{"absolute_path":"/work/app/b.go"}`, note: "Edit b.go"},
		{tool: "write_file", input: `{"file_path":"/work/app/README.md"}`, note: "Write README.md"},
		{tool: "create_file", input: `{"file_path":"notes/n.txt"}`, note: "Write notes/n.txt"},
		{tool: "run_terminal_command", input: `{"command":"go test ./...\nsecond"}`, note: "Run: go test ./..."},
		{tool: "run_shell_command", input: `{"command":"go    test"}`, note: "Run: go test"},
		{tool: "Shell", input: `{"command":"ls"}`, note: "Run: ls"},
		{tool: "exec_command", input: `{"command":"curl token=supersecret"}`, note: "Run: curl token=[redacted]"},
		{tool: "grep_search", input: `{"pattern":"foo"}`, note: "Grep foo"},
		{tool: "search_file_content", input: `{"query":"bar"}`, note: "Grep bar"},
		{tool: "list_dir", input: `{"path":"/work/app/src"}`, note: "List src"},
		{tool: "list_directory", input: `{"target_file":"/work/app/lib"}`, note: "List lib"},
		{tool: "glob", input: `{"pattern":"**/*.go"}`, note: "List **/*.go"},
		{tool: "web_fetch", input: `{"url":"https://user:pass@example.com/private?token=abc"}`, note: "WebFetch example.com"},
		{tool: "web_search", input: `{"query":"statuspan hooks"}`, note: "WebSearch statuspan hooks"},
		{tool: "google_web_search", input: `{"query":"statuspan"}`, note: "WebSearch statuspan"},
		{tool: "notebook_edit", input: `{"notebook_path":"/work/app/notes/n.ipynb"}`, note: "notebook_edit notes/n.ipynb"},
		{tool: "CustomTool", input: `{}`, note: "CustomTool"},
		{tool: "CustomTool", input: `{"command":"echo hi"}`, note: "Run: echo hi"},
	}
	for _, tc := range cases {
		t.Run(tc.tool+" "+tc.note, func(t *testing.T) {
			useGrokFailMarkerDir(t)
			in := ev("PreToolUse")
			in.Cwd = flexString(cwd)
			in.ToolName = flexString(tc.tool)
			in.ToolInput = []byte(tc.input)
			got, ok := grokUpdate(in, "Grok")
			if !ok || noteOf(got) != tc.note {
				t.Fatalf("ok %v note %q want %q", ok, noteOf(got), tc.note)
			}
		})
	}
}

func TestGrokEnvOverridesTool(t *testing.T) {
	clearProxy(t)
	ts, rec := newPostServer(t)
	payload := `{"session_id":"abcdef1234567890","cwd":"/work/My Project","hook_event_name":"UserPromptSubmit","prompt":"please fix the login bug"}`
	t.Setenv("GROK_HOOK_EVENT", "UserPromptSubmit")
	stdout, stderr, err := runHook(t, payload, "hook", "claude", "--url", ts.URL+"/api/jobs")
	if err != nil || stdout != "" || stderr != "" {
		t.Fatalf("err %v stdout %q stderr %q", err, stdout, stderr)
	}
	hits := rec.snapshot()
	if len(hits) != 1 {
		t.Fatalf("hits %+v", hits)
	}
	body := hits[0].body
	if body["agent"] != "Grok" || body["job_id"] != grokJob || body["state"] != "running" {
		t.Fatalf("body %+v", body)
	}

	rec.reset()
	stdout, stderr, err = runHook(t, payload, "hook", "cursor", "--url", ts.URL+"/api/jobs")
	if err != nil || stdout != "" || stderr != "" {
		t.Fatalf("cursor err %v stdout %q stderr %q", err, stdout, stderr)
	}
	hits = rec.snapshot()
	if len(hits) != 1 || hits[0].body["agent"] != "Grok" || hits[0].body["job_id"] != grokJob {
		t.Fatalf("cursor hits %+v", hits)
	}
}

func TestUnknownHookTool(t *testing.T) {
	clearProxy(t)
	ts, rec := newPostServer(t)
	payload := `{"session_id":"abcdef1234567890","cwd":"/work/My Project","hook_event_name":"Stop"}`
	stdout, stderr, err := runHook(t, payload, "hook", "opencode", "--url", ts.URL+"/api/jobs")
	if err != nil || stdout != "" || stderr != "" || len(rec.snapshot()) != 0 {
		t.Fatalf("err %v stdout %q stderr %q hits %d", err, stdout, stderr, len(rec.snapshot()))
	}
	for _, args := range [][]string{
		{"hooks", "install", "opencode"},
		{"hooks", "uninstall", "windsurf"},
		{"hooks", "print", "aider"},
		{"hooks", "install"},
	} {
		_, _, err := runCLI(t, args...)
		var ee *exitError
		if !errors.As(err, &ee) || ee.code != 2 {
			t.Fatalf("%v err %v", args, err)
		}
		if !strings.Contains(err.Error(), "claude") || !strings.Contains(err.Error(), "grok") || !strings.Contains(err.Error(), "codex") || !strings.Contains(err.Error(), "cursor") || !strings.Contains(err.Error(), "gemini") {
			t.Fatalf("%v err %v", args, err)
		}
	}
}

func TestGrokHooksInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "statuspan.json")
	bin := "/tmp/statuspan"
	wantCmd := bin + " hook grok --url http://127.0.0.1:9/api/jobs"

	stdout, stderr, err := runCLI(t, "hooks", "install", "grok", "--settings", path, "--bin", bin, "--url", "http://127.0.0.1:9/api/jobs", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if stderr != "" || !strings.HasSuffix(stdout, "\n") || !strings.Contains(stdout, wantCmd) {
		t.Fatalf("dry-run stdout %s stderr %q", stdout, stderr)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("dry-run wrote the file")
	}

	stdout, stderr, err = runCLI(t, "hooks", "install", "grok", "--settings", path, "--bin", bin, "--url", "http://127.0.0.1:9/api/jobs")
	if err != nil || stdout != "" {
		t.Fatalf("err %v stdout %q stderr %q", err, stdout, stderr)
	}
	if !strings.Contains(stderr, "Updated "+path) || strings.Contains(stderr, "hooks-trust") {
		t.Fatalf("stderr %q", stderr)
	}
	installed := mustRead(t, path)
	assertGrokHookFile(t, installed, wantCmd)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}

	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "hooks", "install", "grok", "--settings", path, "--bin", bin, "--url", "http://127.0.0.1:9/api/jobs"); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode after rewrite %o", info.Mode().Perm())
	}
	if strings.Count(string(mustRead(t, path)), wantCmd) != 8 {
		t.Fatal("duplicate install")
	}
	if n := len(backupFiles(t, path)); n != 1 {
		t.Fatalf("backups %d", n)
	}

	foreign := filepath.Join(dir, "foreign.json")
	original := []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo mine"}]}]}}`)
	if err := os.WriteFile(foreign, original, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = runCLI(t, "hooks", "install", "grok", "--settings", foreign, "--bin", bin)
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("foreign err %v", err)
	}
	if got := mustRead(t, foreign); string(got) != string(original) {
		t.Fatalf("foreign changed %s", got)
	}
	if n := len(backupFiles(t, foreign)); n != 0 {
		t.Fatalf("foreign backups %d", n)
	}
	_, _, err = runCLI(t, "hooks", "install", "grok", "--settings", foreign, "--bin", bin, "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("dry-run foreign err %v", err)
	}

	_, stderr, err = runCLI(t, "hooks", "uninstall", "grok", "--settings", path, "--bin", bin)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "Removed "+path) {
		t.Fatalf("uninstall stderr %q", stderr)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("uninstall left the file")
	}
	stdout, stderr, err = runCLI(t, "hooks", "uninstall", "grok", "--settings", path, "--bin", bin)
	if err != nil || stdout != "" || !strings.Contains(stderr, "No hooks file") {
		t.Fatalf("missing err %v stdout %q stderr %q", err, stdout, stderr)
	}
	_, _, err = runCLI(t, "hooks", "uninstall", "grok", "--settings", foreign, "--bin", bin)
	if err == nil || !strings.Contains(err.Error(), "refusing to remove") {
		t.Fatalf("foreign uninstall err %v", err)
	}
	if got := mustRead(t, foreign); string(got) != string(original) {
		t.Fatal("foreign uninstall changed the file")
	}

	_, _, err = runCLI(t, "hooks", "install", "grok", "--settings", path, "--token", "sekret", "--bin", bin)
	if err == nil || !strings.Contains(err.Error(), "--token-file") || !strings.Contains(err.Error(), "STATUSPAN_TOKEN") {
		t.Fatalf("token err %v", err)
	}

	home := t.TempDir()
	project := t.TempDir()
	if _, _, err := runCLI(t, "hooks", "install", "grok", "--home", home, "--bin", bin); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".grok", "hooks", "statuspan.json")); err != nil {
		t.Fatal(err)
	}
	_, stderr, err = runCLI(t, "hooks", "install", "grok", "--project", "--dir", project, "--bin", bin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(project, ".grok", "hooks", "statuspan.json")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "/hooks-trust") || !strings.Contains(stderr, "--trust") {
		t.Fatalf("project stderr %q", stderr)
	}

	stdout, stderr, err = runCLI(t, "hooks", "print", "grok", "--bin", bin, "--settings", filepath.Join(dir, "print.json"), "--home", dir, "--dir", dir)
	if err != nil || stderr != "" || !strings.HasSuffix(stdout, "\n") || !strings.Contains(stdout, bin+" hook grok") {
		t.Fatalf("print err %v stderr %q stdout %s", err, stderr, stdout)
	}
	if _, err := os.Stat(filepath.Join(dir, "print.json")); !os.IsNotExist(err) {
		t.Fatal("print wrote settings")
	}
	if _, err := os.Stat(filepath.Join(dir, ".grok")); !os.IsNotExist(err) {
		t.Fatal("print created .grok")
	}
}

func assertGrokHookFile(t *testing.T, data []byte, wantCmd string) {
	t.Helper()
	doc := decodeSettings(t, data)
	hooks, _ := doc["hooks"].(map[string]any)
	if len(doc) != 1 {
		t.Fatalf("extra keys %+v", doc)
	}
	if strings.Count(string(data), wantCmd) != 8 {
		t.Fatalf("command count\n%s", data)
	}
	if strings.Contains(string(data), `"async"`) {
		t.Fatalf("async set\n%s", data)
	}
	check := func(event, matcher string, timeout float64) {
		t.Helper()
		groups, _ := hooks[event].([]any)
		if len(groups) != 1 {
			t.Fatalf("%s groups %+v", event, groups)
		}
		gm, _ := groups[0].(map[string]any)
		if matcher == "" {
			if _, ok := gm["matcher"]; ok {
				t.Fatalf("%s matcher %+v", event, gm["matcher"])
			}
		} else if gm["matcher"] != matcher {
			t.Fatalf("%s matcher %+v", event, gm["matcher"])
		}
		hs, _ := gm["hooks"].([]any)
		if len(hs) != 1 {
			t.Fatalf("%s hooks %+v", event, hs)
		}
		hm, _ := hs[0].(map[string]any)
		if hm["type"] != "command" || hm["command"] != wantCmd || hm["timeout"] != timeout {
			t.Fatalf("%s handler %+v", event, hm)
		}
	}
	check("SessionStart", "", 3)
	check("UserPromptSubmit", "", 3)
	check("PreToolUse", "*", 3)
	check("Notification", "permission_prompt", 3)
	check("Stop", "", 5)
	check("StopFailure", "", 5)
	check("StopCancelled", "", 5)
	check("SessionEnd", "", 3)
}

func useGrokFailMarkerDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := grokFailMarkerDir
	grokFailMarkerDir = dir
	t.Cleanup(func() { grokFailMarkerDir = prev })
	return dir
}
