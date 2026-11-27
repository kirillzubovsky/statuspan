package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kirillzubovsky/statuspan/internal/store"
)

const (
	testCwd     = "/work/My Project"
	testSession = "abcdef1234567890"
	testJob     = "claude-my-project-abcdef12"
)

func ev(event string) claudeHookInput {
	return claudeHookInput{
		SessionID:     testSession,
		Cwd:           testCwd,
		HookEventName: flexString(event),
	}
}

func TestClaudeHookEvents(t *testing.T) {
	hundred := 100
	cases := []struct {
		name     string
		in       claudeHookInput
		agent    string
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
			title:    "My Project: Claude Code session",
			note:     "Session started (startup)",
		},
		{
			name:     "session start resume stays queued",
			in:       with(ev("SessionStart"), func(in *claudeHookInput) { in.Source = "resume" }),
			state:    "queued",
			hasTitle: true,
			title:    "My Project: Claude Code session",
			note:     "Session started (resume)",
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
			name: "pre tool use omits title",
			in: with(ev("PreToolUse"), func(in *claudeHookInput) {
				in.ToolName = "Read"
				in.ToolInput = json.RawMessage(`{"file_path":"/work/My Project/main.go"}`)
			}),
			state: "running",
			note:  "Read main.go",
		},
		{
			name: "post tool use ignored",
			in:   ev("PostToolUse"),
			skip: true,
		},
		{
			name: "post tool use failure ignored",
			in:   ev("PostToolUseFailure"),
			skip: true,
		},
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
			name: "elicitation dialog blocks",
			in: with(ev("Notification"), func(in *claudeHookInput) {
				in.NotificationType = "elicitation_dialog"
				in.Message = "pick one"
			}),
			state: "blocked",
			note:  "Needs your input: pick one",
		},
		{
			name: "elicitation url dialog blocks",
			in: with(ev("Notification"), func(in *claudeHookInput) {
				in.NotificationType = "elicitation_url_dialog"
				in.Message = "open the link"
			}),
			state: "blocked",
			note:  "Needs your input: open the link",
		},
		{
			name: "idle notification ignored",
			in:   with(ev("Notification"), func(in *claudeHookInput) { in.NotificationType = "idle_prompt" }),
			skip: true,
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
			name:  "stop waits on background tasks",
			in:    with(ev("Stop"), func(in *claudeHookInput) { in.BackgroundTasks = json.RawMessage(`[{},{}]`) }),
			state: "running",
			note:  "Waiting on 2 background task(s)",
		},
		{
			name: "stop hook active ignored",
			in:   with(ev("Stop"), func(in *claudeHookInput) { in.StopHookActive = true }),
			skip: true,
		},
		{
			name: "subagent stop ignored",
			in:   with(ev("Stop"), func(in *claudeHookInput) { in.AgentID = "agent_1" }),
			skip: true,
		},
		{
			name: "stop failure",
			in: with(ev("StopFailure"), func(in *claudeHookInput) {
				in.Error = "overloaded"
				in.ErrorDetails = json.RawMessage(`"rate limit"`)
			}),
			state: "failed",
			note:  "API error: overloaded rate limit",
		},
		{
			name:  "session end",
			in:    with(ev("SessionEnd"), func(in *claudeHookInput) { in.Reason = "logout" }),
			state: "done",
			note:  "Session ended (logout)",
		},
		{
			name: "unknown event ignored",
			in:   ev("PreCompact"),
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
			useFailMarkerDir(t)
			agent := tc.agent
			if agent == "" {
				agent = "Claude Code"
			}
			got, ok := claudeUpdate(tc.in, agent)
			if tc.skip {
				if ok {
					t.Fatalf("posted %+v", got)
				}
				return
			}
			if !ok {
				t.Fatal("no post")
			}
			if got.Agent != agent || got.JobID != testJob || got.State != tc.state {
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
			note := ""
			if got.Note != nil {
				note = *got.Note
			}
			if note != tc.note {
				t.Fatalf("note %q", note)
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

func TestSessionStartSeqBackdated(t *testing.T) {
	useFailMarkerDir(t)
	start := with(ev("SessionStart"), func(in *claudeHookInput) { in.Source = "startup" })
	prompt := with(ev("UserPromptSubmit"), func(in *claudeHookInput) { in.Prompt = "please fix the login bug" })

	before := nextSeq()
	got, ok := claudeUpdate(start, "Claude Code")
	after := nextSeq()
	if !ok || got.Seq == nil {
		t.Fatal("session start posted nothing")
	}
	assigned := *got.Seq + sessionStartSeqBackdate
	if assigned <= before || assigned >= after {
		t.Fatalf("session seq %d is not nextSeq()-2000 between %d and %d", *got.Seq, before, after)
	}

	// The prompt is stored first. A SessionStart that shows up later must
	// still carry an older seq, or the board keeps the generic title.
	promptUp, ok := claudeUpdate(prompt, "Claude Code")
	if !ok || promptUp.Seq == nil {
		t.Fatal("prompt posted nothing")
	}
	late, ok := claudeUpdate(start, "Claude Code")
	if !ok || late.Seq == nil {
		t.Fatal("late session start posted nothing")
	}
	if *late.Seq >= *promptUp.Seq {
		t.Fatalf("late SessionStart seq %d >= prompt seq %d", *late.Seq, *promptUp.Seq)
	}
	if *promptUp.Seq-*late.Seq > sessionStartSeqBackdate {
		t.Fatalf("skew %d exceeds %d", *promptUp.Seq-*late.Seq, sessionStartSeqBackdate)
	}
}

func TestClaudeJobID(t *testing.T) {
	cases := []struct {
		cwd     string
		session string
		want    string
		ok      bool
	}{
		{cwd: "/work/My Project", session: "abcdef1234567890", want: "claude-my-project-abcdef12", ok: true},
		{cwd: "/work/My Project/", session: "abcdef1234567890", want: "claude-my-project-abcdef12", ok: true},
		{cwd: "/tmp/My Cool Project!!!", session: "AbC-99zz-REST", want: "claude-my-cool-project-abc-99zz", ok: true},
		{cwd: "/tmp/" + strings.Repeat("a", 40), session: "abcdef1234567890", want: "claude-" + strings.Repeat("a", 32) + "-abcdef12", ok: true},
		{cwd: "/tmp/x", session: "ab_cd_ef_ghXXXX", want: "claude-x-ab-cd-ef", ok: true},
		{cwd: "/", session: "abcdef1234567890", want: "claude-dir-abcdef12", ok: true},
		{cwd: "/tmp/x", session: "", ok: false},
		{cwd: "/tmp/x", session: "----", ok: false},
	}
	for _, tc := range cases {
		got, ok := claudeJobID(tc.cwd, tc.session)
		if ok != tc.ok || got != tc.want {
			t.Errorf("claudeJobID(%q, %q) = %q, %v; want %q, %v", tc.cwd, tc.session, got, ok, tc.want, tc.ok)
		}
	}
	useFailMarkerDir(t)
	in := ev("SessionEnd")
	in.Cwd = "/"
	in.Reason = "clear"
	got, ok := claudeUpdate(in, "")
	if !ok || got.Agent != "Claude Code" || got.JobID != "claude-dir-abcdef12" {
		t.Fatalf("%+v ok %v", got, ok)
	}
	if got.Title != nil {
		t.Fatalf("title %q", *got.Title)
	}
	if got.Note == nil || *got.Note != "Session ended (clear)" || got.Progress != nil {
		t.Fatalf("note %+v progress %+v", got.Note, got.Progress)
	}
}

func TestPromptTitles(t *testing.T) {
	pasted := "<pasted_content name=\"x\">\ntoken=should-not-appear\n</pasted_content>\nplease fix the login bug"
	tagged := "<b>please</b> fix the login bug now"
	shortPaste := "<pasted_content>\n" + strings.Repeat("secret word ", 20) + "</pasted_content>\nyes"
	cases := []struct {
		prompt string
		title  string
		ok     bool
	}{
		{prompt: "yes", ok: false},
		{prompt: "continue", ok: false},
		{prompt: "go ahead", ok: false},
		{prompt: "  yes  ", ok: false},
		{prompt: "/review", ok: false},
		{prompt: "/review the whole module now", ok: false},
		{prompt: "fix the bug", title: "fix the bug", ok: true},
		{prompt: "please fix the login bug", title: "please fix the login bug", ok: true},
		{prompt: pasted, title: "please fix the login bug", ok: true},
		{prompt: tagged, title: "please fix the login bug now", ok: true},
		{prompt: shortPaste, ok: false},
	}
	for _, tc := range cases {
		got, ok := promptTitle(tc.prompt)
		if ok != tc.ok || got != tc.title {
			t.Errorf("promptTitle(%q) = %q, %v; want %q, %v", tc.prompt, got, ok, tc.title, tc.ok)
		}
		if strings.Contains(got, "should-not-appear") || strings.Contains(got, "secret") {
			t.Fatalf("leaked pasted content %q", got)
		}
	}
	if strings.Contains(stripMarkup(pasted), "should-not-appear") {
		t.Fatal("pasted block survived")
	}
	words := strings.TrimSpace(strings.Repeat("alpha ", 20))
	title, ok := promptTitle(words)
	if !ok || len([]rune(title)) > hookTitleMax || !strings.HasSuffix(title, "…") || !strings.HasSuffix(strings.TrimSuffix(title, "…"), "alpha") {
		t.Fatalf("word boundary title %q", title)
	}
	long := strings.Repeat("x", 90) + " a b"
	title, ok = promptTitle(long)
	if !ok || !strings.HasSuffix(title, "…") || len([]rune(title)) != hookTitleMax {
		t.Fatalf("hard cut %q (%d)", title, len([]rune(title)))
	}
	exact := "fix the " + strings.Repeat("b", 72)
	title, ok = promptTitle(exact)
	if !ok || title != exact || strings.Contains(title, "…") {
		t.Fatalf("exact 80 %q", title)
	}
}

func TestClaudeToolNotes(t *testing.T) {
	cwd := "/work/app"
	cases := []struct {
		name  string
		tool  string
		input string
		agent string
		note  string
	}{
		{name: "edit relative", tool: "Edit", input: `{"file_path":"/work/app/src/main.go"}`, note: "Edit src/main.go"},
		{name: "write relative", tool: "Write", input: `{"file_path":"/work/app/README.md"}`, note: "Write README.md"},
		{name: "multiedit relative", tool: "MultiEdit", input: `{"file_path":"/work/app/a/b.go"}`, note: "MultiEdit a/b.go"},
		{name: "notebook relative", tool: "NotebookEdit", input: `{"notebook_path":"/work/app/notes/n.ipynb"}`, note: "NotebookEdit notes/n.ipynb"},
		{name: "read outside", tool: "Read", input: `{"file_path":"/elsewhere/secret.txt"}`, note: "Read secret.txt"},
		{name: "read already relative", tool: "Read", input: `{"file_path":"lib/x.go"}`, note: "Read lib/x.go"},
		{name: "read spaced", tool: "Read", input: `{"file_path":"/work/app/my file.go"}`, note: "Read my file.go"},
		{name: "bash first line", tool: "Bash", input: `{"command":"go test ./...\nsecond"}`, note: "Bash: go test ./..."},
		{name: "bash collapsed", tool: "Bash", input: `{"command":"go    test"}`, note: "Bash: go test"},
		{name: "bash secret", tool: "Bash", input: `{"command":"curl token=supersecret"}`, note: "Bash: curl token=[redacted]"},
		{name: "bash truncated", tool: "Bash", input: `{"command":"` + strings.Repeat("x", 100) + `"}`, note: "Bash: " + strings.Repeat("x", 80)},
		{name: "grep", tool: "Grep", input: `{"pattern":"foo"}`, note: "Grep foo"},
		{name: "glob", tool: "Glob", input: `{"pattern":"**/*.go"}`, note: "Glob **/*.go"},
		{name: "grep truncated", tool: "Grep", input: `{"pattern":"` + strings.Repeat("p", 100) + `"}`, note: truncateRunes("Grep "+strings.Repeat("p", 100), 60, false)},
		{name: "webfetch host", tool: "WebFetch", input: `{"url":"https://example.com/private/path?token=abc"}`, note: "WebFetch example.com"},
		{name: "webfetch userinfo", tool: "WebFetch", input: `{"url":"https://user:pass@example.com/x"}`, note: "WebFetch example.com"},
		{name: "websearch", tool: "WebSearch", input: `{"query":"statuspan hooks"}`, note: "WebSearch statuspan hooks"},
		{name: "websearch truncated", tool: "WebSearch", input: `{"query":"` + strings.Repeat("q", 80) + `"}`, note: truncateRunes("WebSearch "+strings.Repeat("q", 80), 60, false)},
		{name: "task", tool: "Task", input: `{"description":"research the api"}`, note: "Subagent: research the api"},
		{name: "agent tool", tool: "Agent", input: `{"description":"review the diff"}`, note: "Subagent: review the diff"},
		{name: "todo", tool: "TodoWrite", input: `{"todos":[]}`, note: "Updating todo list"},
		{name: "mcp", tool: "mcp__github__create_issue", input: `{}`, note: "MCP github: create_issue"},
		{name: "other", tool: "SomethingElse", input: `{}`, note: "SomethingElse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := ev("PreToolUse")
			in.Cwd = flexString(cwd)
			in.ToolName = flexString(tc.tool)
			in.ToolInput = json.RawMessage(tc.input)
			if tc.agent != "" {
				in.AgentID = flexString(tc.agent)
			}
			got, ok := claudeUpdate(in, "Claude Code")
			if !ok {
				t.Fatal("no post")
			}
			if got.State != "running" || got.Title != nil || got.Progress != nil {
				t.Fatalf("state %s title %+v progress %+v", got.State, got.Title, got.Progress)
			}
			if got.Note == nil || *got.Note != tc.note {
				t.Fatalf("note %q want %q", noteOf(got), tc.note)
			}
			if strings.Contains(tc.name, "webfetch") && (strings.Contains(*got.Note, "private") || strings.Contains(*got.Note, "token") || strings.Contains(*got.Note, "user")) {
				t.Fatalf("url leaked %q", *got.Note)
			}
		})
	}

	sub := ev("PreToolUse")
	sub.Cwd = "/work/app"
	sub.AgentID = "agent_9"
	sub.ToolName = "Bash"
	sub.ToolInput = json.RawMessage(`{"command":"ls"}`)
	got, ok := claudeUpdate(sub, "Claude Code")
	if !ok || got.Note == nil || *got.Note != "Subagent: Bash: ls" {
		t.Fatalf("subagent bash %+v", got.Note)
	}
	sub.ToolName = "Task"
	sub.ToolInput = json.RawMessage(`{"description":"research the api"}`)
	got, ok = claudeUpdate(sub, "Claude Code")
	if !ok || got.Note == nil || *got.Note != "Subagent: research the api" {
		t.Fatalf("subagent task %+v", got.Note)
	}
}

func TestRedactSecrets(t *testing.T) {
	pem := "before\n-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEAaaaaaaaaaaaaaaaaaaaaaaaa\n-----END RSA PRIVATE KEY-----\nafter"
	jwt := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "token equals", in: "token=supersecret", want: "token=[redacted]"},
		{name: "password colon", in: "password: hunter2", want: "password: [redacted]"},
		{name: "api_key", in: "api_key=abc", want: "api_key=[redacted]"},
		{name: "api-key", in: "api-key=abc", want: "api-key=[redacted]"},
		{name: "apikey", in: "apikey=abc", want: "apikey=[redacted]"},
		{name: "passwd", in: "passwd=abc", want: "passwd=[redacted]"},
		{name: "export", in: "export FOO_TOKEN=sekret", want: "export FOO_TOKEN=[redacted]"},
		{name: "bearer", in: "Bearer abcdef", want: "Bearer [redacted]"},
		{name: "authorization bearer", in: "authorization: Bearer abc.def", want: "authorization: [redacted]"},
		{name: "userinfo", in: "see https://user:pass@example.com/a for docs", want: "see https://[redacted]@example.com/a for docs"},
		{name: "query", in: "https://example.com/a?token=supersecret&page=1", want: "https://example.com/a?token=[redacted]&page=1"},
		{name: "sk", in: "key sk-abcdefghijklmnopqrst end", want: "key [redacted] end"},
		{name: "sk short", in: "sk-short stays", want: "sk-short stays"},
		{name: "ghp", in: "ghp_1234567890abcdef", want: "[redacted]"},
		{name: "gho", in: "gho_1234567890abcdef", want: "[redacted]"},
		{name: "ghs", in: "ghs_1234567890abcdef", want: "[redacted]"},
		{name: "github pat", in: "github_pat_1234567890abcdef", want: "[redacted]"},
		{name: "slack", in: "xoxb-1234567890", want: "[redacted]"},
		{name: "akia", in: "AKIAIOSFODNN7EXAMPLE", want: "[redacted]"},
		{name: "aiza", in: "AIza012345678901234567890123456789", want: "[redacted]"},
		{name: "jwt", in: jwt, want: "[redacted]"},
		{name: "hex", in: "zz 0123456789abcdef0123456789abcdef zz", want: "zz [redacted] zz"},
		{name: "hex short", in: "0123456789abcdef0123456789abcde", want: "0123456789abcdef0123456789abcde"},
		{name: "base64", in: "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdef", want: "[redacted]"},
		{name: "base64 short", in: "ABCDEFGHIJKLMNOPQRSTUVWXYZabcde", want: "ABCDEFGHIJKLMNOPQRSTUVWXYZabcde"},
		{name: "nul join", in: "a\x00b", want: "ab"},
		{name: "nul secret", in: "token=\x00sekret", want: "token=[redacted]"},
		{name: "prose", in: "please fix the login bug in the parser", want: "please fix the login bug in the parser"},
		{name: "token word", in: "the token is fine", want: "the token is fine"},
		{name: "password word", in: "enter your password please", want: "enter your password please"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactSecrets(tc.in); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
	got := redactSecrets(pem)
	if strings.Contains(got, "PRIVATE KEY") || strings.Contains(got, "MIIE") || !strings.Contains(got, "[redacted]") || !strings.Contains(got, "before") || !strings.Contains(got, "after") {
		t.Fatalf("pem %q", got)
	}
	head := redactSecrets("-----BEGIN OPENSSH PRIVATE KEY-----")
	if strings.Contains(head, "PRIVATE KEY") || head != "[redacted]" {
		t.Fatalf("header %q", head)
	}
}

func TestHookPostsAndIgnoresBadInput(t *testing.T) {
	clearProxy(t)
	ts, rec := newPostServer(t)
	payload := `{"session_id":"abcdef1234567890","cwd":"/work/My Project","hook_event_name":"UserPromptSubmit","prompt":"please fix the login bug"}`
	stdout, stderr, err := runHook(t, payload, "hook", "claude", "--url", ts.URL+"/api/jobs")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "" || stderr != "" {
		t.Fatalf("stdout %q stderr %q", stdout, stderr)
	}
	hits := rec.snapshot()
	if len(hits) != 1 {
		t.Fatalf("hits %+v", hits)
	}
	body := hits[0].body
	if body["agent"] != "Claude Code" || body["job_id"] != testJob || body["state"] != "running" {
		t.Fatalf("body %+v", body)
	}
	if body["title"] != "please fix the login bug" || body["note"] != "Working on the prompt" {
		t.Fatalf("text %+v", body)
	}
	if body["ttl"] != float64(hookTTL) {
		t.Fatalf("ttl %v", body["ttl"])
	}
	if _, ok := body["progress"]; ok {
		t.Fatalf("progress set %+v", body["progress"])
	}
	seq, ok := body["seq"].(float64)
	if !ok || seq <= 0 {
		t.Fatalf("seq %v", body["seq"])
	}
	if hits[0].path != "/api/jobs" || hits[0].auth != "" {
		t.Fatalf("path %s auth %q", hits[0].path, hits[0].auth)
	}

	rec.reset()
	stdout, stderr, err = runHook(t, "", "hook", "claude", "--url", ts.URL+"/api/jobs")
	if err != nil || stdout != "" || stderr != "" || len(rec.snapshot()) != 0 {
		t.Fatalf("empty stdin err %v out %q errOut %q hits %d", err, stdout, stderr, len(rec.snapshot()))
	}
	stdout, stderr, err = runHook(t, "{", "hook", "claude", "--url", ts.URL+"/api/jobs")
	if err != nil || stdout != "" || stderr != "" || len(rec.snapshot()) != 0 {
		t.Fatalf("bad json err %v out %q errOut %q hits %d", err, stdout, stderr, len(rec.snapshot()))
	}
	stdout, stderr, err = runHook(t, "{", "hook", "claude", "--verbose", "--url", ts.URL+"/api/jobs")
	if err != nil || stdout != "" || !strings.Contains(stderr, "statuspan:") {
		t.Fatalf("verbose err %v out %q errOut %q", err, stdout, stderr)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"ok":false,"error":"nope"}`, http.StatusBadRequest)
	}))
	t.Cleanup(bad.Close)
	stdout, _, err = runHook(t, payload, "hook", "claude", "--url", bad.URL+"/api/jobs")
	if err != nil || stdout != "" {
		t.Fatalf("4xx err %v stdout %q", err, stdout)
	}

	const token = "write-token-secret"
	tokPath := filepath.Join(t.TempDir(), "write-token")
	if err := os.WriteFile(tokPath, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	relay, relayRec := newPostServer(t)
	stdout, stderr, err = runHook(t, payload, "hook", "claude", "--relay", relay.URL, "--workspace", "home", "--token-file", tokPath)
	if err != nil || stdout != "" {
		t.Fatalf("relay err %v stdout %q stderr %q", err, stdout, stderr)
	}
	rh := relayRec.snapshot()
	if len(rh) != 1 || rh[0].auth != "Bearer "+token || rh[0].path != "/v1/w/home/jobs" {
		t.Fatalf("relay hits %+v", rh)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	stdout, _, err = runHook(t, payload, "hook", "claude", "--url", "http://"+addr+"/api/jobs")
	elapsed := time.Since(start)
	if err != nil || stdout != "" {
		t.Fatalf("down err %v stdout %q", err, stdout)
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("daemon down took %s", elapsed)
	}
}

func TestHooksInstallMerge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	original := []byte(`{
  "theme": "dark",
  "editor": {"tabSize": 2},
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {"type": "command", "command": "'/tmp/My Apps/statuspan' hook claude --url http://127.0.0.1:9/api/jobs --addr 127.0.0.1:14477 --token-file /tmp/tok", "async": true}
        ]
      }
    ],
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {"type": "command", "command": "echo keep-me"},
          {"type": "command", "command": "echo hook claude"}
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
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
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	bin := "/tmp/My Apps/statuspan"
	args := []string{
		"hooks", "install", "claude",
		"--settings", path,
		"--bin", bin,
		"--url", "http://127.0.0.1:9/api/jobs",
		"--addr", "127.0.0.1:14477",
		"--token-file", "/tmp/tok",
	}
	wantCmd := "'/tmp/My Apps/statuspan' hook claude --url http://127.0.0.1:9/api/jobs --addr 127.0.0.1:14477 --token-file /tmp/tok"

	stdout, stderr, err := runCLI(t, append(append([]string{}, args...), "--dry-run")...)
	if err != nil {
		t.Fatal(err)
	}
	if stderr != "" {
		t.Fatalf("dry-run stderr %q", stderr)
	}
	if !strings.HasSuffix(stdout, "\n") || !json.Valid([]byte(stdout)) {
		t.Fatalf("dry-run stdout %s", stdout)
	}
	if !strings.Contains(stdout, wantCmd) || !strings.Contains(stdout, `"theme": "dark"`) {
		t.Fatalf("dry-run missing fields %s", stdout)
	}
	afterDry, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterDry, original) {
		t.Fatal("dry-run wrote the file")
	}
	if n := len(backupFiles(t, path)); n != 0 {
		t.Fatalf("dry-run backups %d", n)
	}

	stdout, stderr, err = runCLI(t, args...)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "" {
		t.Fatalf("install stdout %q", stdout)
	}
	if !strings.Contains(stderr, "Restart Claude Code sessions to pick up the hooks.") || !strings.Contains(stderr, "Backup ") || !strings.Contains(stderr, path) {
		t.Fatalf("stderr %q", stderr)
	}
	backups := backupFiles(t, path)
	if len(backups) != 1 {
		t.Fatalf("backups %+v", backups)
	}
	saved, err := os.ReadFile(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(saved, original) {
		t.Fatal("backup is not the original file")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	installed := mustRead(t, path)
	assertInstalledHooks(t, installed, wantCmd)
	if strings.Contains(string(installed), "--token ") || strings.Contains(string(installed), "/tmp/statuspan hook claude --addr 1") {
		t.Fatalf("unexpected command in %s", installed)
	}

	if _, _, err := runCLI(t, args...); err != nil {
		t.Fatal(err)
	}
	again := mustRead(t, path)
	assertInstalledHooks(t, again, wantCmd)
	if strings.Count(string(again), wantCmd) != 7 {
		t.Fatalf("duplicate install\n%s", again)
	}

	if _, stderr, err = runCLI(t, "hooks", "uninstall", "claude", "--settings", path, "--bin", bin); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "Restart Claude Code sessions to pick up the hooks.") {
		t.Fatalf("uninstall stderr %q", stderr)
	}
	removed := mustRead(t, path)
	if strings.Contains(string(removed), wantCmd) || strings.Contains(string(removed), "hook claude --addr 1") {
		t.Fatalf("uninstall left our hook\n%s", removed)
	}
	doc := decodeSettings(t, removed)
	if doc["theme"] != "dark" {
		t.Fatalf("theme lost %+v", doc["theme"])
	}
	raw, _ := json.Marshal(doc)
	if !strings.Contains(string(raw), "echo keep-me") || !strings.Contains(string(raw), "echo hook claude") {
		t.Fatalf("unrelated hooks lost %s", raw)
	}
}

func TestHooksInstallEdges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	original := []byte("{nope")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := runCLI(t, "hooks", "install", "claude", "--settings", path, "--bin", "/tmp/statuspan")
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("err %v", err)
	}
	if got := mustRead(t, path); !bytes.Equal(got, original) {
		t.Fatalf("invalid json changed %q", got)
	}
	if n := len(backupFiles(t, path)); n != 0 {
		t.Fatalf("backup on invalid json %d", n)
	}

	kept := mustRead(t, path)
	_, _, err = runCLI(t, "hooks", "install", "claude", "--settings", path, "--token", "sekret", "--bin", "/tmp/statuspan")
	if err == nil || !strings.Contains(err.Error(), "--token-file") || !strings.Contains(err.Error(), "STATUSPAN_TOKEN") {
		t.Fatalf("token err %v", err)
	}
	_, _, err = runCLI(t, "hooks", "install", "claude", "--settings", path, "--token=sekret")
	if err == nil {
		t.Fatal("token= form accepted")
	}
	if got := mustRead(t, path); !bytes.Equal(got, kept) {
		t.Fatal("token flag wrote the file")
	}

	arrayPath := filepath.Join(dir, "array.json")
	if err := os.WriteFile(arrayPath, []byte(`{"hooks":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "hooks", "install", "claude", "--settings", arrayPath, "--bin", "/tmp/statuspan"); err == nil {
		t.Fatal("hooks array accepted")
	}
	if got := mustRead(t, arrayPath); string(got) != `{"hooks":[]}` {
		t.Fatalf("array file changed %s", got)
	}

	fresh := filepath.Join(dir, "fresh.json")
	if _, _, err := runCLI(t, "hooks", "install", "claude", "--settings", fresh, "--bin", "/tmp/statuspan"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("new mode %o", info.Mode().Perm())
	}
	if _, _, err := runCLI(t, "hooks", "uninstall", "claude", "--settings", fresh, "--bin", "/tmp/statuspan"); err != nil {
		t.Fatal(err)
	}
	doc := decodeSettings(t, mustRead(t, fresh))
	if _, ok := doc["hooks"]; ok {
		t.Fatalf("hooks key remained %+v", doc)
	}

	home := t.TempDir()
	project := t.TempDir()
	if _, _, err := runCLI(t, "hooks", "install", "claude", "--home", home, "--bin", "/tmp/statuspan"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "hooks", "install", "claude", "--project", "--dir", project, "--bin", "/tmp/statuspan"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(project, ".claude", "settings.json")); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing-uninstall.json")
	stdout, stderr, err := runCLI(t, "hooks", "uninstall", "claude", "--settings", missing, "--bin", "/tmp/statuspan")
	if err != nil || stdout != "" || !strings.Contains(stderr, "No settings file") {
		t.Fatalf("err %v out %q errOut %q", err, stdout, stderr)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("uninstall created a file")
	}

	stdout, stderr, err = runCLI(t, "hooks", "print", "claude", "--bin", "/tmp/statuspan", "--settings", filepath.Join(dir, "print.json"), "--home", dir, "--dir", dir)
	if err != nil || stderr != "" || !strings.HasSuffix(stdout, "\n") || !json.Valid([]byte(stdout)) || !strings.Contains(stdout, "/tmp/statuspan hook claude") {
		t.Fatalf("print err %v stderr %q stdout %s", err, stderr, stdout)
	}
	if _, err := os.Stat(filepath.Join(dir, "print.json")); !os.IsNotExist(err) {
		t.Fatal("print wrote settings")
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude")); !os.IsNotExist(err) {
		t.Fatal("print created .claude")
	}
}

func TestSplitShellWords(t *testing.T) {
	got := splitShellWords("'/tmp/My Apps/statuspan' hook claude --addr 127.0.0.1:14477")
	want := []string{"/tmp/My Apps/statuspan", "hook", "claude", "--addr", "127.0.0.1:14477"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%#v", got)
	}
	if !isStatuspanClaudeHook("'/tmp/My Apps/statuspan' hook claude", "/tmp/statuspan") {
		t.Fatal("quoted statuspan not recognized")
	}
	if isStatuspanClaudeHook("echo hook claude", "/tmp/statuspan") {
		t.Fatal("echo matched")
	}
}

func assertInstalledHooks(t *testing.T, data []byte, wantCmd string) {
	t.Helper()
	doc := decodeSettings(t, data)
	if doc["theme"] != "dark" {
		t.Fatalf("theme %+v", doc["theme"])
	}
	editor, _ := doc["editor"].(map[string]any)
	if editor["tabSize"] != float64(2) {
		t.Fatalf("editor %+v", doc["editor"])
	}
	text := string(data)
	if strings.Count(text, "echo keep-me") != 1 || strings.Count(text, "echo hook claude") != 1 {
		t.Fatalf("unrelated hooks changed\n%s", text)
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
				if hm["command"] == wantCmd {
					found = gm
					handler := hm
					if handler["type"] != "command" {
						t.Fatalf("%s type %+v", event, handler["type"])
					}
					_, hasAsync := handler["async"]
					if async {
						if handler["async"] != true {
							t.Fatalf("%s async %+v", event, handler["async"])
						}
					} else if hasAsync {
						t.Fatalf("%s has async", event)
					}
					gotTimeout, hasTimeout := handler["timeout"]
					if timeout == 0 {
						if hasTimeout {
							t.Fatalf("%s timeout %v", event, gotTimeout)
						}
					} else if gotTimeout != timeout {
						t.Fatalf("%s timeout %v", event, gotTimeout)
					}
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
	check("Notification", "permission_prompt|elicitation_dialog|elicitation_url_dialog", true, 0)
	check("Stop", "", false, 5)
	check("StopFailure", "", false, 5)
	check("SessionEnd", "", false, 3)
}

func TestStopFailureSessionEndStaysFailed(t *testing.T) {
	dir := useFailMarkerDir(t)
	fail := with(ev("StopFailure"), func(in *claudeHookInput) {
		in.Error = "authentication_failed"
	})
	got, ok := claudeUpdate(fail, "Claude Code")
	if !ok || got.State != "failed" || noteOf(got) != "API error: authentication_failed" {
		t.Fatalf("stop failure ok %v state %s note %q", ok, got.State, noteOf(got))
	}
	marker := filepath.Join(dir, slugify(testSession, 0)+".failed")
	info, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Size() != 0 {
		t.Fatalf("marker mode %o size %d", info.Mode().Perm(), info.Size())
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %o", dirInfo.Mode().Perm())
	}

	end := with(ev("SessionEnd"), func(in *claudeHookInput) { in.Reason = "other" })
	got, ok = claudeUpdate(end, "Claude Code")
	if !ok || got.State != "failed" || got.Progress != nil || noteOf(got) != "Session ended after an API error (other)" {
		t.Fatalf("session end ok %v state %s note %q progress %+v", ok, got.State, noteOf(got), got.Progress)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker after session end: %v", err)
	}
}

func TestStopFailureNextTurnEndsDone(t *testing.T) {
	dir := useFailMarkerDir(t)
	fail := with(ev("StopFailure"), func(in *claudeHookInput) {
		in.Error = "authentication_failed"
	})
	if _, ok := claudeUpdate(fail, "Claude Code"); !ok {
		t.Fatal("stop failure posted nothing")
	}
	marker := filepath.Join(dir, slugify(testSession, 0)+".failed")
	if _, err := os.Stat(marker); err != nil {
		t.Fatal(err)
	}

	prompt := with(ev("UserPromptSubmit"), func(in *claudeHookInput) { in.Prompt = "please try the login again" })
	got, ok := claudeUpdate(prompt, "Claude Code")
	if !ok || got.State != "running" {
		t.Fatalf("prompt ok %v state %s", ok, got.State)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker after prompt: %v", err)
	}

	// The next turn failed again, and Stop (done) is what clears it this time.
	if _, ok := claudeUpdate(fail, "Claude Code"); !ok {
		t.Fatal("second stop failure posted nothing")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal(err)
	}
	got, ok = claudeUpdate(ev("Stop"), "Claude Code")
	if !ok || got.State != "done" || noteOf(got) != "Turn finished" {
		t.Fatalf("stop ok %v state %s note %q", ok, got.State, noteOf(got))
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker after stop: %v", err)
	}

	end := with(ev("SessionEnd"), func(in *claudeHookInput) { in.Reason = "other" })
	got, ok = claudeUpdate(end, "Claude Code")
	if !ok || got.State != "done" || noteOf(got) != "Session ended (other)" {
		t.Fatalf("session end ok %v state %s note %q", ok, got.State, noteOf(got))
	}
}

func useFailMarkerDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := claudeFailMarkerDir
	claudeFailMarkerDir = dir
	t.Cleanup(func() { claudeFailMarkerDir = prev })
	return dir
}

func with(in claudeHookInput, fn func(*claudeHookInput)) claudeHookInput {
	fn(&in)
	return in
}

func noteOf(u store.Update) string {
	if u.Note == nil {
		return ""
	}
	return *u.Note
}

func decodeSettings(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func backupFiles(t *testing.T, path string) []string {
	t.Helper()
	matches, err := filepath.Glob(path + ".statuspan-backup-*")
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func clearProxy(t *testing.T) {
	t.Helper()
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(key, "")
	}
}

func runCLI(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	return captureOutputs(t, func() error { return run(args) })
}

func runHook(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr string
	var runErr error
	err := withStdin(t, stdin, func() error {
		var callErr error
		stdout, stderr, callErr = captureOutputs(t, func() error { return run(args) })
		runErr = callErr
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return stdout, stderr, runErr
}

func withStdin(t *testing.T, data string, fn func() error) error {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	defer func() {
		os.Stdin = old
		_ = r.Close()
	}()
	go func() {
		_, _ = w.Write([]byte(data))
		_ = w.Close()
	}()
	return fn()
}

func captureOutputs(t *testing.T, fn func() error) (string, string, error) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout = outW
	os.Stderr = errW
	var outBuf, errBuf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = outBuf.ReadFrom(outR)
	}()
	go func() {
		defer wg.Done()
		_, _ = errBuf.ReadFrom(errR)
	}()
	runErr := fn()
	os.Stdout = oldOut
	os.Stderr = oldErr
	_ = outW.Close()
	_ = errW.Close()
	wg.Wait()
	return outBuf.String(), errBuf.String(), runErr
}
