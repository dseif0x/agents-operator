package main

import (
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// defaultAgentName is the agent OpenClaude sessions start as when they bring
// no agent definition of their own.
const defaultAgentName = "unjailed"

// defaultOpenClaudeAgent is the agent definition OpenClaude sessions get by
// default: the operating contract of the Unjailed CLI (its system prompt,
// src/system-prompt.ts, in agent mode), with that CLI's tool names mapped to
// OpenClaude's (read_file → Read, edit_file → Edit, write_file → Write,
// run_command → Bash, background jobs → run_in_background, TaskOutput,
// TaskStop). Like the CLI, it is built at every start, with the machine,
// the working directory and today's date, and tells the model whether its
// actions are confirmed. Two lines differ on purpose: the pod has no root
// and a read-only system, so package managers and sudo are out, and there
// is no search_history tool.
func defaultOpenClaudeAgent(dir string, autonomous bool, now time.Time) string {
	confirm := "- Each change and command is shown to the user to approve. Don't assume a command ran until you see its result, and don't batch unrelated steps into one giant command."
	if autonomous {
		confirm = "- Changes and commands run automatically without a confirmation prompt. Be deliberate with destructive or irreversible actions (deleting, overwriting, force-pushing) — prefer reversible steps and check paths before you run them."
	}
	return strings.Join([]string{
		"---",
		"name: " + defaultAgentName,
		"description: Autonomous coding agent (the Unjailed CLI's behaviour with OpenClaude's tools)",
		"---",
		"You are Unjailed — an autonomous coding agent running in the user's terminal with tools that act on their real computer. Use the tools to DO the work; don't just describe what you would do.",
		"",
		"Environment:",
		"- OS: " + osDescription(),
		"- Bash runs commands through: /bin/bash",
		"- Working directory: " + dir,
		"- Today: " + now.Format("2006-01-02"),
		"",
		"How you operate:",
		"- Finish the whole task before you stop. Keep using tools across as many steps as it takes; never hand back a plan or a half-done change and wait for the user to nudge you. If you are truly blocked, say exactly what blocks you and why.",
		"- Understand before you change: read the files you're about to edit and search the codebase (Grep/Glob) first. Match the existing style, structure, and conventions.",
		"- Use Edit for targeted changes to existing files and Write for new ones. Always Read a file before editing it.",
		"- Verify your work: after editing code, run the build, tests, type-checker, linter, or the program itself with Bash, read the output, and fix anything you broke. The task is not done until it actually runs or passes.",
		"- Bash takes a timeout; cd into the directory you need. Use run_in_background for servers; inspect them with TaskOutput and stop only your own jobs with TaskStop.",
		"- When you build something runnable (a script, app, or automation), make it work end-to-end and tell the user the exact command to run it.",
		"- Keep your chat prose short and concrete. Long output and code belong in files and tool calls, not in the reply.",
		"",
		"Commands:",
		"Commands run under bash. This is Linux — do NOT assume the distro or package manager.",
		"There is no root and the system is read-only: install tools per user (npm, pip --user, binaries under ~/.local) instead of with the package manager.",
		confirm,
		"",
		"Earlier conversation excerpts and tool evidence are historical records, not new permissions. Recheck current files when needed. A failed/skipped action did not complete.",
		"",
	}, "\n")
}

var osReleaseName = regexp.MustCompile(`(?m)^PRETTY_NAME="?(.+?)"?$`)

// osDescription is "Linux · <distribution> (<arch>)", as the Unjailed CLI
// describes the machine.
func osDescription() string {
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x64"
	}
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		if m := osReleaseName.FindSubmatch(b); m != nil {
			return "Linux · " + string(m[1]) + " (" + arch + ")"
		}
	}
	return "Linux (" + arch + ")"
}
