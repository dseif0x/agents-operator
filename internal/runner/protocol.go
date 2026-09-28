// Package runner holds the wire protocol shared between the hub and the
// agent-runner process that lives inside every session pod.
//
// The WebSocket protocol is deliberately tiny:
//
//   - binary frames carry raw PTY bytes in both directions, with no framing;
//   - text frames carry small JSON control messages (see Control).
//
// The same protocol is spoken between browser and hub, and between hub and
// runner. The hub never inspects binary frames.
package runner

import "time"

// Port is the TCP port the runner listens on inside the pod.
const Port = 7681

// Default PTY size used until the first client sends a resize.
const (
	DefaultCols = 220
	DefaultRows = 50
)

// ScrollbackSize is the size of the runner's ring buffer of raw PTY output.
const ScrollbackSize = 2 << 20 // 2 MiB

// Ping cadence shared by every WebSocket hop.
const (
	PingInterval = 20 * time.Second
	PingMisses   = 3
)

// Control message types (the "t" field).
const (
	// Client to server.
	MsgResize      = "resize"       // {"t":"resize","cols":220,"rows":50}
	MsgRestart     = "restart"      // {"t":"restart"}
	MsgExportLogin = "export_login" // {"t":"export_login","kind":"claude_login"} — hub only

	// Server to client.
	MsgHello = "hello" // {"t":"hello","scrollback":true,"cols":220,"rows":50}
	MsgExit  = "exit"  // {"t":"exit","code":0}
	MsgLogin = "login" // {"t":"login","kind":"claude_login","data":"<base64>"} — reply to export_login
	MsgError = "error" // {"t":"error","message":"..."}
)

// Control is the JSON payload of every text frame.
type Control struct {
	T          string `json:"t"`
	Cols       int    `json:"cols,omitempty"`
	Rows       int    `json:"rows,omitempty"`
	Code       *int   `json:"code,omitempty"`
	Scrollback bool   `json:"scrollback,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Data       string `json:"data,omitempty"`
	Message    string `json:"message,omitempty"`
}

// Status is the JSON body of GET /status on the runner. The hub polls it
// every ten seconds to drive the session list.
type Status struct {
	Agent            string     `json:"agent"`
	AgentPID         int        `json:"agent_pid"`
	Running          bool       `json:"running"`
	ExitCode         *int       `json:"exit_code,omitempty"`
	Restarts         int        `json:"restarts"`
	StartedAt        time.Time  `json:"started_at"`
	LastOutputAt     *time.Time `json:"last_output_at,omitempty"`
	BytesSinceAttach int64      `json:"bytes_since_attach"`
	Cols             int        `json:"cols"`
	Rows             int        `json:"rows"`
	Clients          int        `json:"clients"`
	// NeedsAttention is true when the agent has gone quiet with the
	// terminal ending in something that looks like a prompt.
	NeedsAttention bool `json:"needs_attention"`
	// Tail is the last visible line of output, ANSI stripped, for list pages.
	Tail string `json:"tail,omitempty"`
}

// Agent identifiers understood by the runner image.
const (
	AgentClaude   = "claude"
	AgentOpenCode = "opencode"
	AgentCodex    = "codex"
	// AgentShell is a plain bash shell. It is not an AI agent but is handy
	// for debugging a pod and for tests.
	AgentShell = "shell"
)

// Agents lists every agent the runner image understands.
var Agents = []string{AgentClaude, AgentOpenCode, AgentCodex, AgentShell}

// ValidAgent reports whether name is one of Agents.
func ValidAgent(name string) bool {
	for _, a := range Agents {
		if a == name {
			return true
		}
	}
	return false
}

// Login kinds that can be exported from a running session and seeded into
// a new one.
const (
	LoginClaude = "claude_login"
	LoginCodex  = "codex_login"
)

// LoginFile maps a login kind to the path, relative to HOME, of the CLI's
// credential file.
var LoginFile = map[string]string{
	LoginClaude: ".claude/.credentials.json",
	LoginCodex:  ".codex/auth.json",
}

// Environment variables the runner reads. All of them are injected by the
// hub through the per-session Secret or the pod spec.
const (
	EnvRunnerToken   = "RUNNER_TOKEN"
	EnvAgent         = "AGENT"
	EnvAutonomous    = "AUTONOMOUS"
	EnvRepos         = "REPOS"    // JSON list of {url, branch, path}; cloned under WORKSPACE
	EnvGitHubToken   = "GH_TOKEN" // GitHub token for gh and https clones of github.com
	EnvGitUserName   = "GIT_USER_NAME"
	EnvGitUserEmail  = "GIT_USER_EMAIL"
	EnvGitSSHKey     = "GIT_SSH_KEY"             // private key contents
	EnvGitHTTPSToken = "GIT_HTTPS_TOKEN"         // token for https clones
	EnvSeedPrefix    = "AGENTS_OPERATOR_LOGIN_"  // + upper(kind): base64 credential file to seed
	EnvWorkspace     = "WORKSPACE"               // defaults to /workspace
	EnvListen        = "RUNNER_LISTEN"           // defaults to :7681
	EnvSessionName   = "AGENTS_OPERATOR_SESSION" // human name, used for the prompt/hostname
)
