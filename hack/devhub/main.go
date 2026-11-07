// Command devhub runs the hub's API and UI against an in-memory store, with
// each session's runner as a local agent-runner process instead of a pod.
// It is for trying the UI with real agent CLIs on a laptop or in a dev pod:
//
//	go build -o /tmp/agent-runner ./cmd/agent-runner
//	go run -tags ui ./hack/devhub -runner /tmp/agent-runner -env-file dev.env
//
// Log in as dev / dev. -env-file holds KEY=VALUE lines every runner gets
// (a model endpoint for the CLIs: ANTHROPIC_BASE_URL, ANTHROPIC_API_KEY,
// ANTHROPIC_MODEL, AGENTS_OPERATOR_AI_ACCOUNTS, ...).
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dseif0x/agents-operator/internal/api"
	"github.com/dseif0x/agents-operator/internal/auth"
	"github.com/dseif0x/agents-operator/internal/config"
	"github.com/dseif0x/agents-operator/internal/github"
	"github.com/dseif0x/agents-operator/internal/runner"
	"github.com/dseif0x/agents-operator/internal/session"
	"github.com/dseif0x/agents-operator/internal/store"
	"github.com/dseif0x/agents-operator/internal/term"
	"github.com/dseif0x/agents-operator/internal/ui"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8090", "address to serve on")
	runnerBin := flag.String("runner", "agent-runner", "the agent-runner binary")
	envFile := flag.String("env-file", "", "KEY=VALUE lines every runner gets")
	dataDir := flag.String("data", "", "where runners keep their homes and workspaces (default: a temp dir)")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(*listen, *runnerBin, *envFile, *dataDir, log); err != nil {
		log.Error("devhub", "err", err)
		os.Exit(1)
	}
}

func run(listen, runnerBin, envFile, dataDir string, log *slog.Logger) error {
	ctx := context.Background()
	if dataDir == "" {
		d, err := os.MkdirTemp("", "devhub-")
		if err != nil {
			return err
		}
		dataDir = d
	}
	extra, err := readEnv(envFile)
	if err != nil {
		return err
	}
	st := store.NewMemory()
	hash, _ := auth.HashPassword("dev")
	if _, err := st.Users().UpsertPassword(ctx, "dev", hash); err != nil {
		return err
	}
	pods := &localPods{bin: runnerBin, dir: dataDir, env: extra, log: log, procs: map[string]*localPod{}}
	proxy := &term.Proxy{Resolver: pods, OriginPatterns: []string{listen, "localhost:*", "127.0.0.1:*"}, Log: log}
	svc := &session.Service{
		Store: st, Term: proxy, Broker: session.NewBroker(), GitHub: &github.Client{}, Log: log,
		Defaults: session.Defaults{PVCSize: "1Gi", Autonomous: true},
	}
	pods.svc = svc
	svc.Orch = pods
	svc.Uplinks = session.NewUplinks(svc, pods, log)
	go svc.RunPoller(ctx, 3*time.Second)

	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	srv := &api.Server{
		Cfg: &config.Config{AllowedHosts: []string{listen, "localhost", "127.0.0.1"}}, Store: st, Sessions: svc,
		Auth:    auth.PasswordAuthenticator{Users: st.Users()},
		Cookies: auth.NewSessions(secret, false, st.Users()),
		Limiter: auth.NewRateLimiter(100, time.Minute),
		Term:    proxy,
		UI:      ui.Handler(),
		Log:     log,
	}
	log.Info("devhub listening; log in as dev / dev", "addr", listen, "data", dataDir)
	return (&http.Server{Addr: listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}).ListenAndServe()
}

func readEnv(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && !strings.HasPrefix(line, "#") && strings.Contains(line, "=") {
			out = append(out, line)
		}
	}
	return out, sc.Err()
}

// localPods stands in for the reconciler: a session that should run gets an
// agent-runner process, one that should stop loses it.
type localPods struct {
	bin   string
	dir   string
	env   []string
	log   *slog.Logger
	svc   *session.Service
	mu    sync.Mutex
	procs map[string]*localPod
}

type localPod struct {
	cmd   *exec.Cmd
	port  int
	token string
}

func (p *localPods) Endpoint(_ context.Context, id string) (term.Endpoint, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	lp := p.procs[id]
	if lp == nil {
		return term.Endpoint{}, term.ErrNotRunning
	}
	return term.Endpoint{IP: "127.0.0.1", Port: lp.port, Token: lp.token}, nil
}

func (p *localPods) PodLogs(context.Context, string, int64) ([]byte, error) {
	return []byte("(devhub: runner logs go to its stderr)\n"), nil
}

func (p *localPods) Notify(id string) { go p.converge(id) }

func (p *localPods) converge(id string) {
	ctx := context.Background()
	sess, err := p.svc.Store.Sessions().Get(ctx, id)
	if err != nil {
		return
	}
	switch sess.State {
	case store.StateCreating:
		if err := p.start(sess); err != nil {
			p.log.Error("cannot start a runner", "session", id, "err", err)
			s, _ := p.svc.Store.Sessions().SetState(ctx, id, store.StateFailed, err.Error())
			p.svc.SessionChanged(ctx, s)
			return
		}
		s, _ := p.svc.Store.Sessions().SetState(ctx, id, store.StateRunning, "")
		p.svc.SessionChanged(ctx, s)
		p.svc.PollOnce(ctx)
	case store.StateStopping:
		p.kill(id)
		s, _ := p.svc.Store.Sessions().SetState(ctx, id, store.StateStopped, "")
		p.svc.SessionChanged(ctx, s)
		p.svc.PollOnce(ctx)
	case store.StateDeleting:
		p.kill(id)
		_ = p.svc.Store.Sessions().Delete(ctx, id)
		p.svc.SessionDeleted(ctx, id, sess.OwnerID)
	}
}

func (p *localPods) start(sess *store.Session) error {
	p.kill(sess.ID)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	hl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	hookPort := hl.Addr().(*net.TCPAddr).Port
	_ = hl.Close()
	var tok [16]byte
	_, _ = rand.Read(tok[:])
	token := hex.EncodeToString(tok[:])
	root := filepath.Join(p.dir, sess.ID)
	home, ws := filepath.Join(root, "home"), filepath.Join(root, "workspace")
	for _, d := range []string{home, ws} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	cmd := exec.Command(p.bin)
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		runner.EnvWorkspace+"="+ws,
		runner.EnvListen+"="+fmt.Sprintf("127.0.0.1:%d", port),
		runner.EnvHookListen+"="+fmt.Sprintf("127.0.0.1:%d", hookPort),
		runner.EnvHookURL+"="+fmt.Sprintf("http://127.0.0.1:%d/hook", hookPort),
		runner.EnvRunnerToken+"="+token,
		runner.EnvAgent+"="+sess.Agent,
		runner.EnvAutonomous+"="+fmt.Sprint(sess.Autonomous),
		runner.EnvInterface+"="+sess.Interface,
		runner.EnvSessionName+"="+sess.Name,
		runner.EnvHandle+"="+sess.Handle,
		runner.EnvPersona+"="+sess.Persona,
		runner.EnvRepos+"=[]",
	)
	cmd.Env = append(cmd.Env, p.env...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	p.mu.Lock()
	p.procs[sess.ID] = &localPod{cmd: cmd, port: port, token: token}
	p.mu.Unlock()
	go func() { _ = cmd.Wait() }()
	// Wait until it answers.
	for range 100 {
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond); err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("the runner does not listen on %d", port)
}

func (p *localPods) kill(id string) {
	p.mu.Lock()
	lp := p.procs[id]
	delete(p.procs, id)
	p.mu.Unlock()
	if lp != nil && lp.cmd.Process != nil {
		_ = syscall.Kill(-lp.cmd.Process.Pid, syscall.SIGTERM)
	}
}
