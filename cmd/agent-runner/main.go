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
	os.Exit(run())
}

// run is main without os.Exit, so deferred cleanups run.
func run() int {
	var (
		listen    = flag.String("listen", envOr(runner.EnvListen, fmt.Sprintf(":%d", runner.Port)), "address to serve the WebSocket on")
		workspace = flag.String("workspace", envOr(runner.EnvWorkspace, "/workspace"), "workspace directory (the PVC mount)")
		agent     = flag.String("agent", envOr(runner.EnvAgent, runner.AgentShell), "agent to run: "+strings.Join(runner.Agents, ", "))
		cmdLine   = flag.String("cmd", "", "override the agent command line (debugging and tests)")
		skipBoot  = flag.Bool("skip-bootstrap", false, "do not prepare the workspace or clone the repo")
		logLevel  = flag.String("log-level", envOr("LOG_LEVEL", "info"), "log level")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLevel(*logLevel)}))
	slog.SetDefault(log)

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

	autonomous := strings.EqualFold(os.Getenv(runner.EnvAutonomous), "true") || os.Getenv(runner.EnvAutonomous) == "1"
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

	srv := &server.Server{Proc: proc, Token: token, Agent: *agent, Home: ws.HomeDir(), LoginBaseline: loginBaseline, Log: log}
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
	default:
		return server.Command{Path: "bash", Args: []string{"-l"}, Dir: dir, Env: env}
	}
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
}

// HomeDir is the agent's HOME on the PVC.
func (w *Workspace) HomeDir() string { return filepath.Join(w.Root, "home") }

// WorkDir is the directory the agent starts in: the workspace root, where
// AGENTS.md lives and every repository is a subdirectory. Falls back to the
// current directory when the root does not exist.
func (w *Workspace) WorkDir() string {
	if st, err := os.Stat(w.Root); err == nil && st.IsDir() {
		return w.Root
	}
	return ""
}
