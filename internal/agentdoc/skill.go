// Package agentdoc is the skill and discovery document embedded in the CLI.
package agentdoc

import _ "embed"

//go:embed SKILL.md
var skillMD string

// Skill is the full agent skill, including YAML frontmatter.
func Skill() string { return skillMD }

// Prompt is a short snippet for AGENTS.md or CLAUDE.md. It has no frontmatter.
func Prompt() string {
	return `StatusPan tracks finite jobs on a local board. Push status. It does not call you back.

Post work that has a finish line. Keep healthy recurring jobs off the board (cron, daily digests, watchers). If a recurring job becomes blocked or failed, post it so it shows under Needs attention, then post done once it is resolved. Use blocked only when you actually asked a human for something.

A failed or unreachable StatusPan must not block, delay, or fail the real work.

Wrap a command. StatusPan exits with the command's exit code:

    statuspan run --agent "My Agent" --job build-1 --title "Build v1" -- make test

Post one update. Omit --seq and the CLI sends epoch milliseconds:

    statuspan post --agent "My Agent" --job build-1 --state running --progress 40 --note "halfway"

Do not send stalled or abandoned. They are derived. Run statuspan agent for the full skill, or statuspan agent install to install it.
`
}
