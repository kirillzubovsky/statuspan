package agentdoc

import "encoding/json"

// Discovery is the machine-readable description of this CLI.
// Field order is the JSON object order.
type Discovery struct {
	Name           string     `json:"name"`
	Version        string     `json:"version"`
	Description    string     `json:"description"`
	Docs           Docs       `json:"docs"`
	Commands       []Command  `json:"commands"`
	Env            []EnvVar   `json:"env"`
	Endpoints      []Endpoint `json:"endpoints"`
	States         States     `json:"states"`
	PayloadFields  []Field    `json:"payload_fields"`
	ReportingRules []string   `json:"reporting_rules"`
}

// Docs are the public spec URLs.
type Docs struct {
	SkillMD     string `json:"skill_md"`
	LLMsTxt     string `json:"llms_txt"`
	LLMsFullTxt string `json:"llms_full_txt"`
	OpenAPIJSON string `json:"openapi_json"`
}

// Command is one CLI entry point.
type Command struct {
	Name  string `json:"name"`
	Usage string `json:"usage"`
}

// EnvVar is one environment variable the CLI reads.
type EnvVar struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Endpoint is one HTTP path an agent posts to or reads.
type Endpoint struct {
	Name        string `json:"name"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description"`
}

// States splits values an agent may send from values the board derives.
type States struct {
	Sendable []string `json:"sendable"`
	Derived  []string `json:"derived"`
}

// Field is one job-update property.
type Field struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// NewDiscovery builds the document for version, which comes from the binary.
func NewDiscovery(version string) Discovery {
	return Discovery{
		Name:        "statuspan",
		Version:     version,
		Description: "Local board for which AI agent is working on what. Agents push finite jobs. StatusPan does not call them back.",
		Docs: Docs{
			SkillMD:     "https://statuspan.com/skill.md",
			LLMsTxt:     "https://statuspan.com/llms.txt",
			LLMsFullTxt: "https://statuspan.com/llms-full.txt",
			OpenAPIJSON: "https://statuspan.com/openapi.json",
		},
		Commands: []Command{
			{Name: "serve", Usage: "statuspan [serve] [--addr host:port] [--data dir] [--abandon-after DUR] [--abandon-waiting-after DUR] [--relay-url URL] [--relay-workspace NAME] [--relay-token-file PATH]"},
			{Name: "post", Usage: "statuspan post --agent A --job ID [--state STATE] [--title T] [--progress N] [--link URL] [--note N] [--seq N] [--ttl S] [--url ENDPOINT | --addr host:port | --relay URL --workspace NAME (--token TOKEN | --token-file PATH)]"},
			{Name: "run", Usage: "statuspan run --agent NAME --job ID [--title T] [--ttl SEC] [--heartbeat SEC] [--link URL] [--recurring] [--url ENDPOINT | --addr host:port | --relay URL --workspace NAME (--token TOKEN | --token-file PATH)] [--verbose] -- COMMAND [ARGS...]"},
			{Name: "agent", Usage: "statuspan agent [print|prompt|json|install] [--target LIST] [--dir PATH] [--home PATH] [--dry-run]"},
			{Name: "ls", Usage: "statuspan ls [--addr host:port]"},
			{Name: "clear", Usage: "statuspan clear --yes [--addr host:port]"},
			{Name: "version", Usage: "statuspan version"},
		},
		Env: []EnvVar{
			{Name: "STATUSPAN_AGENT", Description: "Default agent name for statuspan run --agent."},
			{Name: "STATUSPAN_URL", Description: "Full local jobs endpoint for post and run, for example http://127.0.0.1:14477/api/jobs. Overrides STATUSPAN_ADDR when --url and --addr are omitted."},
			{Name: "STATUSPAN_ADDR", Description: "Local daemon host:port. Default 127.0.0.1:4477."},
			{Name: "STATUSPAN_RELAY_URL", Description: "Relay base URL for post and run. With a workspace and a write token, posts go to the relay instead of the local daemon."},
			{Name: "STATUSPAN_RELAY_WORKSPACE", Description: "Relay workspace name, for example home."},
			{Name: "STATUSPAN_TOKEN", Description: "Relay write token for post and run. Not the daemon read token."},
			{Name: "STATUSPAN_TOKEN_FILE", Description: "File containing the relay write token, one line. Used when STATUSPAN_TOKEN and --token and --token-file are unset."},
			{Name: "STATUSPAN_DATA", Description: "Daemon data directory."},
			{Name: "STATUSPAN_RELAY_READ_TOKEN", Description: "Daemon read token used to pull a workspace. Agents do not post with this token."},
			{Name: "STATUSPAN_ABANDON_AFTER", Description: "Silence before a stalled running job is shown as abandoned. Default 24h. 0 disables."},
			{Name: "STATUSPAN_ABANDON_WAITING_AFTER", Description: "Silence before a queued or blocked job is shown as abandoned. Default 168h. 0 disables."},
		},
		Endpoints: []Endpoint{
			{Name: "local_post", Method: "POST", Path: "http://127.0.0.1:4477/api/jobs", Description: "Create or merge job updates on the local daemon. No auth. One object or an array."},
			{Name: "local_list", Method: "GET", Path: "http://127.0.0.1:4477/api/jobs", Description: "List jobs on the local daemon."},
			{Name: "relay_post", Method: "POST", Path: "/v1/w/{workspace}/jobs", Description: "POST $STATUSPAN_RELAY_URL/v1/w/$STATUSPAN_RELAY_WORKSPACE/jobs with Authorization: Bearer and the write token. One object or an array."},
		},
		States: States{
			Sendable: []string{"queued", "running", "blocked", "done", "failed"},
			Derived:  []string{"stalled", "abandoned"},
		},
		PayloadFields: []Field{
			{Name: "agent", Description: "Required. Not blank. No slash, backslash, or NUL."},
			{Name: "job_id", Description: "Required. Same rules as agent. The card is (agent, job_id)."},
			{Name: "title", Description: "Optional label. Omit to leave it. Empty string clears it."},
			{Name: "state", Description: "queued, running, blocked, done, or failed. Required on a new job. Omit later to leave it."},
			{Name: "progress", Description: "Optional integer 0 through 100."},
			{Name: "link", Description: "Optional string. Not validated as a URL. Empty string clears it."},
			{Name: "note", Description: "Optional one-line status. Empty string clears it."},
			{Name: "seq", Description: "Optional integer. A missing seq on raw HTTP always applies. A present seq applies only when it is strictly greater than the stored seq. The CLI fills epoch milliseconds when --seq is omitted."},
			{Name: "ttl", Description: "Optional heartbeat window in seconds, >= 0. Default 600 on create. Running jobs with no applied heartbeat for ttl seconds are stalled."},
		},
		ReportingRules: []string{
			"Post finite jobs that have a finish line.",
			"Keep healthy recurring jobs and routines off the board (cron, daily digests, watchers).",
			"If a recurring job becomes blocked or failed, push it so it shows under Needs attention, then push done once it is resolved.",
			"Use blocked only when you actually asked a human for input or approval.",
			"A failed or unreachable StatusPan must not block, delay, or fail the real work. Use a short timeout and ignore the error.",
			"Do not send stalled or abandoned. The board derives them.",
			"Never print tokens.",
		},
	}
}

// DiscoveryJSON is the indented discovery document plus a trailing newline.
func DiscoveryJSON(version string) ([]byte, error) {
	body, err := json.MarshalIndent(NewDiscovery(version), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}
