// Command agent-runner is the entrypoint of every session pod. It prepares
// the workspace on the PVC, starts the agent CLI under a PTY and serves the
// terminal over a WebSocket for the hub to proxy.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/dseif0x/agents-operator/internal/runner"
	"github.com/dseif0x/agents-operator/internal/runner/server"
	"github.com/dseif0x/agents-operator/internal/store"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "git-credential" {
		gitCredentialHelper(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "init" {
		os.Exit(initMain(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "hook" {
		hookForward(hookURL(), os.Stdin)
		return
	}
	os.Exit(run())
}

// run is main without os.Exit, so deferred cleanups run.
func run() int {
	var (
		listen    = flag.String("listen", envOr(runner.EnvListen, fmt.Sprintf(":%d", runner.Port)), "address to serve the WebSocket on")
		hookAddr  = flag.String("hook-listen", envOr(runner.EnvHookListen, fmt.Sprintf("127.0.0.1:%d", runner.HookPort)), "loopback address that receives agent hook events")
		workspace = flag.String("workspace", envOr(runner.EnvWorkspace, "/workspace"), "workspace directory (the PVC mount)")
		agent     = flag.String("agent", envOr(runner.EnvAgent, runner.AgentShell), "agent to run: "+strings.Join(runner.Agents, ", "))
		cmdLine   = flag.String("cmd", "", "override the agent command line (debugging and tests)")
		skipBoot  = flag.Bool("skip-bootstrap", false, "do not prepare the workspace or clone the repo")
		logLevel  = flag.String("log-level", envOr("LOG_LEVEL", "info"), "log level")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLevel(*logLevel)}))
	slog.SetDefault(log)

	// Before anything can start the agent: take back what init moved out of
	// the environment, and close this process to the agent (private.go).
	if err := loadPrivateEnv(); err != nil {
		log.Error("cannot load the private environment", "err", err)
		return 2
	}
	if err := hideProcess(); err != nil {
		log.Warn("cannot mark the runner non-dumpable; the agent can read its environment", "err", err)
	}

	token := os.Getenv(runner.EnvRunnerToken)
	if token == "" {
		log.Error("RUNNER_TOKEN is required")
		return 2
	}
	if !runner.ValidAgent(*agent) {
		log.Error("unknown agent", "agent", *agent)
		return 2
	}

	ws := &Workspace{Root: *workspace, Agent: *agent, Log: log}
	if !*skipBoot {
		if err := ws.Bootstrap(context.Background()); err != nil {
			log.Error("bootstrap failed", "err", err)
			// Keep serving so the user can see the problem; run a shell instead.
			*agent = runner.AgentShell
		}
	}
	// Login files touched from here on were written by the CLI (a login or a
	// token refresh), not seeded by us; /status reports those to the hub.
	loginBaseline := time.Now()

	autonomous := autonomousEnv()
	cmdFn := func() server.Command {
		if *cmdLine != "" {
			parts := strings.Fields(*cmdLine)
			return server.Command{Path: parts[0], Args: parts[1:], Dir: ws.WorkDir(), Env: ws.Env()}
		}
		return AgentCommand(*agent, autonomous, ws.WorkDir(), ws.Env())
	}

	proc := server.NewProcess(cmdFn, log)
	if err := proc.Start(); err != nil {
		log.Error("failed to start agent", "err", err)
		// Fall back to a shell so the pod stays debuggable.
		fallback := func() server.Command {
			return AgentCommand(runner.AgentShell, false, ws.WorkDir(), ws.Env())
		}
		proc = server.NewProcess(fallback, log)
		if err := proc.Start(); err != nil {
			log.Error("failed to start shell", "err", err)
			return 1
		}
	}
	defer proc.Close()

	hooks := &server.Hooks{Log: log}
	srv := &server.Server{Proc: proc, Token: token, Agent: *agent, Home: ws.HomeDir(), LoginBaseline: loginBaseline, Hooks: hooks, Log: log}
	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", *listen)
	if err != nil {
		log.Error("listen failed", "err", err)
		return 1
	}
	log.Info("agent-runner listening", "addr", ln.Addr().String(), "agent", *agent, "autonomous", autonomous)

	// Agent hook events arrive on the loopback interface only; the agent
	// CLI's hooks post to it through `agent-runner hook`.
	hookSrv := &http.Server{Addr: *hookAddr, Handler: hooks.Handler(), ReadHeaderTimeout: 5 * time.Second}
	if hln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", *hookAddr); err != nil {
		log.Warn("hook listener failed; agent activity will come from the terminal tail", "addr", *hookAddr, "err", err)
	} else {
		go func() {
			if err := hookSrv.Serve(hln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Warn("hook server stopped", "err", err)
			}
		}()
		defer hookSrv.Close()
	}

	// Forward SIGHUP/SIGUSR1 to the agent's process group, so `kubectl exec
	// kill -HUP 1` reaches the CLI.
	fwd := make(chan os.Signal, 4)
	signal.Notify(fwd, syscall.SIGHUP, syscall.SIGUSR1, syscall.SIGUSR2)
	go func() {
		for sig := range fwd {
			if s, ok := sig.(syscall.Signal); ok {
				proc.Signal(s)
			}
		}
	}()

	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	proc.Close()
	return 0
}

// AgentCommand is the one place that knows how to launch each agent CLI.
func AgentCommand(agent string, autonomous bool, dir string, env []string) server.Command {
	switch agent {
	case runner.AgentClaude:
		args := []string{}
		if autonomous {
			args = append(args, "--dangerously-skip-permissions")
		}
		return server.Command{Path: "claude", Args: args, Dir: dir, Env: env}
	case runner.AgentOpenCode:
		return server.Command{Path: "opencode", Args: nil, Dir: dir, Env: env}
	case runner.AgentCodex:
		return server.Command{Path: "codex", Args: nil, Dir: dir, Env: env}
	case runner.AgentOpenClaude:
		args := []string{}
		if autonomous {
			args = append(args, "--dangerously-skip-permissions")
		}
		args = append(args, agentDefinitionArgs(env, dir, autonomous, time.Now())...)
		args = append(args, systemPromptArgs(env)...)
		return server.Command{Path: "openclaude", Args: args, Dir: dir, Env: openClaudeEnv(env)}
	default:
		return server.Command{Path: "bash", Args: []string{"-l"}, Dir: dir, Env: env}
	}
}

// OpenClaude defaults. Unset, it would fall back to its maintainer's own
// gateway (opengateway.gitlawb.com) rather than fail, so the runner always
// names the endpoint.
const (
	openClaudeDefaultBaseURL = "https://unjailedai.com/v1"
	openClaudeDefaultModel   = "glm-5.3"
)

// openClaudeEnv points OpenClaude at the owner's OpenAI-compatible endpoint
// and turns off what it would otherwise send elsewhere. The hub projects the
// account's key and base URL as OPENCLAUDE_API_KEY and OPENCLAUDE_BASE_URL
// (not the OPENAI_* names Codex uses); OPENAI_BASE_URL set in the session's
// own environment wins. The model is the session's OPENAI_MODEL, else the
// last one picked with /model in this session, else the default (see
// openClaudeModel).
func openClaudeEnv(env []string) []string {
	get := func(k string) string {
		for i := len(env) - 1; i >= 0; i-- {
			if key, v, _ := strings.Cut(env[i], "="); key == k {
				return v
			}
		}
		return ""
	}
	out := make([]string, 0, len(env)+10)
	for _, kv := range env {
		// Sentry only starts with a DSN; none should be inherited. The
		// system prompt is on the command line (systemPromptArgs), so every
		// command the agent runs need not carry it too.
		switch k, _, _ := strings.Cut(kv, "="); k {
		case "SENTRY_DSN", runner.EnvSystemPrompt, runner.EnvSystemPromptMode, runner.EnvAgentDefinition, runner.EnvAgentDefinitionName:
		default:
			out = append(out, kv)
		}
	}
	set := func(k, v string) { out = append(out, k+"="+v) }
	set("CLAUDE_CODE_USE_OPENAI", "1")
	baseURL := cmp.Or(get("OPENAI_BASE_URL"), get(runner.EnvOpenClaudeBaseURL), openClaudeDefaultBaseURL)
	set("OPENAI_BASE_URL", baseURL)
	// OpenClaude does not know unjailedai.com's variants and would assume
	// 128k for all of them; give it the sizes unjailedai.com publishes,
	// unless the session sets its own.
	if strings.HasPrefix(baseURL, "https://unjailedai.com/") && get("CLAUDE_CODE_OPENAI_CONTEXT_WINDOWS") == "" {
		if b, err := json.Marshal(unjailedContextWindows); err == nil {
			set("CLAUDE_CODE_OPENAI_CONTEXT_WINDOWS", string(b))
		}
	}
	set("OPENAI_MODEL", cmp.Or(get("OPENAI_MODEL"), savedOpenClaudeModel(get("HOME")), openClaudeDefaultModel))
	if key := get(runner.EnvOpenClaudeAPIKey); key != "" {
		set("OPENAI_API_KEY", key)
	}
	// No telemetry, no self-update (the image is read-only anyway), no
	// marketplace download from Anthropic.
	set("DISABLE_TELEMETRY", "1")
	set("DISABLE_AUTOUPDATER", "1")
	set("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")
	set("CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL", "1")
	return out
}

// systemPromptArgs turns the session's custom system prompt into
// OpenClaude's flag: appended to its own prompt, or replacing it. Passed on
// the command line on every start, so "Restart agent" picks up an edit and
// nothing the agent can write changes it.
func systemPromptArgs(env []string) []string {
	var prompt, mode string
	for _, kv := range env {
		switch k, v, _ := strings.Cut(kv, "="); k {
		case runner.EnvSystemPrompt:
			prompt = v
		case runner.EnvSystemPromptMode:
			mode = v
		}
	}
	if strings.TrimSpace(prompt) == "" {
		return nil
	}
	if mode == "replace" {
		return []string{"--system-prompt", prompt}
	}
	return []string{"--append-system-prompt", prompt}
}

// agentDefinitionArgs writes the session's agent definition, if it has one,
// to ~/.openclaude/agents/<name>.md and returns --agent <name>. Rewritten on
// every start, so the file always matches the session's settings whatever
// the agent did to it.
//
// A session without one starts as the built-in default agent
// (defaultOpenClaudeAgent), unless its system prompt replaces OpenClaude's:
// an agent's prompt would win over that one.
func agentDefinitionArgs(env []string, dir string, autonomous bool, now time.Time) []string {
	var def, name, home, mode string
	for _, kv := range env {
		switch k, v, _ := strings.Cut(kv, "="); k {
		case runner.EnvAgentDefinition:
			def = v
		case runner.EnvAgentDefinitionName:
			name = v
		case runner.EnvSystemPromptMode:
			mode = v
		case "HOME":
			home = v
		}
	}
	if def == "" && mode != "replace" {
		def, name = defaultOpenClaudeAgent(dir, autonomous, now), defaultAgentName
	}
	// The hub validated both; check the name again, it becomes a path.
	if def == "" || home == "" || !agentNameRE.MatchString(name) {
		return nil
	}
	agents := filepath.Join(home, ".openclaude", "agents")
	if err := os.MkdirAll(agents, 0o700); err != nil {
		slog.Warn("cannot write the agent definition; starting without it", "err", err)
		return nil
	}
	if err := os.WriteFile(filepath.Join(agents, name+".md"), []byte(strings.TrimRight(def, "\n")+"\n"), 0o600); err != nil {
		slog.Warn("cannot write the agent definition; starting without it", "err", err)
		return nil
	}
	return []string{"--agent", name}
}

var agentNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// unjailedContextWindows are the context sizes unjailedai.com lists for its
// models (tokens; "1M" and "128K" taken as 1,000,000 and 128,000 to stay
// below the real limit). Models missing here keep OpenClaude's own default.
var unjailedContextWindows = map[string]int{
	"glm-5.3":                   1_000_000,
	"glm-5.3-uncensored":        1_000_000,
	"glm-5.3-abliterated":       1_000_000,
	"glm-5.2":                   1_000_000,
	"glm-5.3-flash":             128_000,
	"glm-5.3-flash-uncensored":  128_000,
	"glm-5.3-flash-abliterated": 128_000,
}

// savedOpenClaudeModel is the model last picked with /model in this
// session: OpenClaude saves it as "model" in ~/.openclaude/settings.json but,
// with its provider configured through the environment, starts from
// OPENAI_MODEL rather than that setting. Handing it back as OPENAI_MODEL
// keeps the choice across restarts. "" when there is none.
func savedOpenClaudeModel(home string) string {
	if home == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(home, ".openclaude", "settings.json"))
	if err != nil {
		return ""
	}
	var settings struct {
		Model any `json:"model"`
	}
	if json.Unmarshal(raw, &settings) != nil {
		return ""
	}
	model, _ := settings.Model.(string)
	model = strings.TrimSpace(model)
	if len(model) > 200 || strings.ContainsAny(model, "\x00\r\n") {
		return ""
	}
	return model
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}

// Workspace is the PVC layout: /workspace/home is HOME and every repo is
// checked out into /workspace/<path>.
type Workspace struct {
	Root string
	// Agent is the CLI this pod runs; some first-run setup is CLI-specific.
	Agent string
	Log   *slog.Logger
	repos []store.Repo
	// cloneKey is the SSH key file the clones use when the agent must not
	// have the key; it exists only during Bootstrap.
	cloneKey string
}

// HomeDir is the agent's HOME on the PVC.
func (w *Workspace) HomeDir() string { return filepath.Join(w.Root, "home") }

// TempDir is the agent's private TMPDIR: a 0700 directory of its own under
// the pod's /tmp, which is scratch and does not survive a restart.
func (w *Workspace) TempDir() string { return filepath.Join(os.TempDir(), "agent") }

// WorkDir is the directory the agent starts in: the workspace root, where
// AGENTS.md lives and every repository is a subdirectory. Falls back to the
// current directory when the root does not exist.
func (w *Workspace) WorkDir() string {
	if st, err := os.Stat(w.Root); err == nil && st.IsDir() {
		return w.Root
	}
	return ""
}
