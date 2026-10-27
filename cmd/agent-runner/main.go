// Command agent-runner is the entrypoint of every session pod. It prepares
// the workspace on the PVC, starts the agent CLI under a PTY and serves the
// terminal over a WebSocket for the hub to proxy.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
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
		hookForward(hookURL(), os.Stdin, os.Stdout)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		os.Exit(mcpMain(os.Stdin, os.Stdout, chatURL()))
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
	chat := &server.Chat{Proc: proc, Agent: *agent, Home: ws.HomeDir(), Roots: []string{ws.Root, ws.TempDir()}, Log: log}
	if *agent == runner.AgentClaude {
		// Only Claude Code reports through hooks; its Stop hook can take
		// chat messages queued during a turn.
		chat.Hooks = hooks
		hooks.OnStop = chat.StopHook
	}
	srv := &server.Server{Proc: proc, Token: token, Agent: *agent, Home: ws.HomeDir(), LoginBaseline: loginBaseline, Hooks: hooks, Chat: chat, Log: log}
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
	// CLI's hooks post to it through `agent-runner hook`, and its chat
	// tools (`agent-runner mcp`) through /chat/.
	loopback := http.NewServeMux()
	loopback.Handle("/hook", hooks.Handler())
	loopback.Handle("/chat/", chat.Handler())
	hookSrv := &http.Server{Addr: *hookAddr, Handler: loopback, ReadHeaderTimeout: 5 * time.Second}
	go chat.Run(ctx)
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
	persona := strings.TrimSpace(envValue(env, runner.EnvPersona))
	switch agent {
	case runner.AgentClaude:
		args := []string{}
		if autonomous {
			args = append(args, "--dangerously-skip-permissions")
		}
		if persona != "" {
			args = append(args, "--append-system-prompt", persona)
		}
		// The chat tools; allowed without a prompt, they only talk to the
		// session's own chats.
		if cfg := mcpConfigFile(envValue(env, "HOME")); cfg != "" {
			args = append(args, "--mcp-config", cfg, "--allowedTools", "mcp__"+mcpServerName)
		}
		return server.Command{Path: "claude", Args: args, Dir: dir, Env: withoutEnv(env, runner.EnvPersona)}
	case runner.AgentOpenCode:
		args := []string{}
		if autonomous {
			args = append(args, "--auto")
		}
		cfg, agentArgs := openCodeConfig(env)
		args = append(args, agentArgs...)
		return server.Command{Path: "opencode", Args: args, Dir: dir, Env: openCodeEnv(env, cfg)}
	case runner.AgentCodex:
		// The persona is in the generated ~/.codex/AGENTS.md (writeAgentsFile);
		// the chat tools are configured on the command line.
		args := []string{"-c", "mcp_servers." + mcpServerName + `.command="agent-runner"`, "-c", "mcp_servers." + mcpServerName + `.args=["mcp"]`}
		return server.Command{Path: "codex", Args: args, Dir: dir, Env: withoutEnv(env, runner.EnvPersona)}
	default:
		return server.Command{Path: "bash", Args: []string{"-l"}, Dir: dir, Env: withoutEnv(env, runner.EnvPersona)}
	}
}

// envValue is the last value of key in env, "" when absent.
func envValue(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, _ := strings.Cut(env[i], "="); k == key {
			return v
		}
	}
	return ""
}

// withoutEnv is env without the given keys.
func withoutEnv(env []string, keys ...string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(keys, k) {
			out = append(out, kv)
		}
	}
	return out
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
	// sshConfig is the ssh config with the owner's SSH keys that git uses
	// (GIT_SSH_COMMAND), "" without keys. When the agent must not have the
	// keys it lies in cloneSSHDir outside HOME, which exists only during
	// Bootstrap.
	sshConfig   string
	cloneSSHDir string
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
