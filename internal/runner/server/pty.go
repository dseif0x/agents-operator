package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/dseif0x/agents-operator/internal/runner"
)

// Command describes how to launch the agent process.
type Command struct {
	Path string
	Args []string
	Dir  string
	Env  []string
}

// Process owns the PTY and the agent process behind it. It multiplexes PTY
// output to any number of subscribers and keeps a scrollback ring buffer.
type Process struct {
	cmdFn func() Command
	log   *slog.Logger

	mu         sync.Mutex
	pty        *os.File
	cmd        *exec.Cmd
	cols, rows int
	running    bool
	exitCode   *int
	restarts   int
	startedAt  time.Time
	lastOutput time.Time
	bytesSince int64
	subs       map[*subscriber]struct{}
	ring       *Ring
	generation int
	closed     bool
	closeOnce  sync.Once
}

// subscriber receives output chunks and exit codes for one attached client.
type subscriber struct {
	out  chan []byte
	exit chan int
}

// NewProcess prepares a process manager. Start must be called to launch it.
func NewProcess(cmdFn func() Command, log *slog.Logger) *Process {
	return &Process{
		cmdFn: cmdFn,
		log:   log,
		cols:  runner.DefaultCols,
		rows:  runner.DefaultRows,
		subs:  map[*subscriber]struct{}{},
		ring:  NewRing(runner.ScrollbackSize),
	}
}

// Start launches the agent under a fresh PTY. It is safe to call again
// after the process has exited (that is what restart does).
func (p *Process) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		return errors.New("agent already running")
	}
	spec := p.cmdFn()
	cmd := exec.Command(spec.Path, spec.Args...) //nolint:noctx // lifetime is managed by Stop/Restart, not a context
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(p.cols), Rows: uint16(p.rows)})
	if err != nil {
		return err
	}
	p.pty = f
	p.cmd = cmd
	p.running = true
	p.exitCode = nil
	p.startedAt = time.Now()
	p.generation++
	gen := p.generation
	p.log.Info("agent started", "pid", cmd.Process.Pid, "path", spec.Path, "args", spec.Args)
	go p.pump(f, cmd, gen)
	return nil
}

// pump copies PTY output to the ring buffer and subscribers until the
// process exits.
func (p *Process) pump(f *os.File, cmd *exec.Cmd, gen int) {
	buf := make([]byte, 32*1024)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			p.broadcast(chunk)
		}
		if err != nil {
			break // EIO when the slave side closes
		}
	}
	err := cmd.Wait()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
		if code < 0 {
			code = 128 + int(exitErr.Sys().(syscall.WaitStatus).Signal())
		}
	} else if err != nil {
		code = 1
	}
	_ = f.Close()
	p.mu.Lock()
	if p.generation != gen {
		p.mu.Unlock()
		return
	}
	p.running = false
	p.exitCode = &code
	subs := make([]*subscriber, 0, len(p.subs))
	for s := range p.subs {
		subs = append(subs, s)
	}
	p.mu.Unlock()
	p.log.Info("agent exited", "code", code)
	for _, s := range subs {
		select {
		case s.exit <- code:
		default:
		}
	}
}

func (p *Process) broadcast(chunk []byte) {
	p.mu.Lock()
	_, _ = p.ring.Write(chunk)
	p.lastOutput = time.Now()
	p.bytesSince += int64(len(chunk))
	subs := make([]*subscriber, 0, len(p.subs))
	for s := range p.subs {
		subs = append(subs, s)
	}
	p.mu.Unlock()
	for _, s := range subs {
		select {
		case s.out <- chunk:
		default:
			// Slow client: drop the chunk for it rather than stalling the PTY.
			// The client can always reconnect and get a fresh replay.
		}
	}
}

// Subscription is one attached client's view of the process.
type Subscription struct {
	// Scrollback is the retained output at the time of attach.
	Scrollback []byte
	// Out receives live output chunks. Slow readers miss chunks.
	Out <-chan []byte
	// Exit receives the exit code each time the agent process exits.
	Exit <-chan int
	// Exited is set when the agent was already down at attach time.
	Exited *int
	cancel func()
}

// Close detaches the subscription.
func (s *Subscription) Close() { s.cancel() }

// Subscribe attaches a client: it gets the current scrollback and then live
// output and exit notifications. The caller must Close the subscription.
func (p *Process) Subscribe() *Subscription {
	sub := &subscriber{out: make(chan []byte, 256), exit: make(chan int, 4)}
	p.mu.Lock()
	p.subs[sub] = struct{}{}
	p.bytesSince = 0
	sb := p.ring.Bytes()
	var exited *int
	if !p.running && p.exitCode != nil {
		c := *p.exitCode
		exited = &c
	}
	p.mu.Unlock()
	return &Subscription{
		Scrollback: sb,
		Out:        sub.out,
		Exit:       sub.exit,
		Exited:     exited,
		cancel: func() {
			p.mu.Lock()
			delete(p.subs, sub)
			p.mu.Unlock()
		},
	}
}

// Scrollback returns a copy of the retained output.
func (p *Process) Scrollback() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ring.Bytes()
}

// Write sends input to the agent's terminal.
func (p *Process) Write(b []byte) error {
	p.mu.Lock()
	f, running := p.pty, p.running
	p.mu.Unlock()
	if !running || f == nil {
		return errors.New("agent not running")
	}
	_, err := f.Write(b)
	return err
}

// Resize changes the PTY size. Sizes are clamped to sane bounds.
func (p *Process) Resize(cols, rows int) error {
	if cols < 2 || rows < 1 || cols > 1000 || rows > 500 {
		return errors.New("invalid size")
	}
	p.mu.Lock()
	p.cols, p.rows = cols, rows
	f, running := p.pty, p.running
	p.mu.Unlock()
	if !running || f == nil {
		return nil
	}
	return pty.Setsize(f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

// Size returns the current PTY size.
func (p *Process) Size() (cols, rows int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cols, p.rows
}

// Restart stops the agent if it is running and launches it again. Scrollback
// is kept so the user can see what happened before.
func (p *Process) Restart(ctx context.Context) error {
	if err := p.Stop(ctx); err != nil {
		return err
	}
	p.mu.Lock()
	p.restarts++
	p.mu.Unlock()
	p.broadcast([]byte("\r\n\x1b[2m[agenthub] restarting agent\x1b[0m\r\n"))
	return p.Start()
}

// Stop terminates the running agent: SIGTERM, then SIGKILL after a grace
// period. It returns once the process has exited.
func (p *Process) Stop(ctx context.Context) error {
	p.mu.Lock()
	if !p.running || p.cmd == nil || p.cmd.Process == nil {
		p.mu.Unlock()
		return nil
	}
	proc := p.cmd.Process
	p.mu.Unlock()
	sub := p.Subscribe()
	defer sub.Close()
	done := sub.Exit
	_ = syscall.Kill(-proc.Pid, syscall.SIGTERM)
	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
	case <-ctx.Done():
		return ctx.Err()
	}
	_ = syscall.Kill(-proc.Pid, syscall.SIGKILL)
	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("agent did not exit after SIGKILL")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Signal forwards a signal to the agent's process group.
func (p *Process) Signal(sig syscall.Signal) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running && p.cmd != nil && p.cmd.Process != nil {
		_ = syscall.Kill(-p.cmd.Process.Pid, sig)
	}
}

// Status builds the status document for GET /status.
func (p *Process) Status(agent string, clients int) runner.Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := runner.Status{
		Agent:            agent,
		Running:          p.running,
		ExitCode:         p.exitCode,
		Restarts:         p.restarts,
		StartedAt:        p.startedAt,
		BytesSinceAttach: p.bytesSince,
		Cols:             p.cols,
		Rows:             p.rows,
		Clients:          clients,
	}
	if p.cmd != nil && p.cmd.Process != nil && p.running {
		st.AgentPID = p.cmd.Process.Pid
	}
	if !p.lastOutput.IsZero() {
		t := p.lastOutput
		st.LastOutputAt = &t
	}
	// Only look at the last few KiB for the tail; enough for one screen.
	b := p.ring.Bytes()
	if len(b) > 8192 {
		b = b[len(b)-8192:]
	}
	st.Tail = LastLine(b)
	if len(st.Tail) > 200 {
		st.Tail = st.Tail[len(st.Tail)-200:]
	}
	st.NeedsAttention = p.running && NeedsAttention(st.Tail, p.lastOutput, time.Now())
	return st
}

// Close stops the agent and releases the PTY.
func (p *Process) Close() {
	p.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		_ = p.Stop(ctx)
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
	})
}

var _ io.Writer = (*Ring)(nil)
