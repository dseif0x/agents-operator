package convo

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Command is how to start a CLI.
type Command struct {
	Path string
	Args []string
	Dir  string
	Env  []string
}

// proc is a CLI child process spoken to with JSON lines over stdio.
type proc struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines chan []byte
	log   *slog.Logger

	wmu    sync.Mutex
	errBuf tail
	done   chan struct{}
	err    error
}

// maxLine bounds one JSON line from a CLI (a tool result can be large).
const maxLine = 64 << 20

func startProc(ctx context.Context, c Command, log *slog.Logger) (*proc, error) {
	cmd := exec.Command(c.Path, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	// Its own process group, so a stop reaches the tools it started too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	p := &proc{cmd: cmd, stdin: stdin, lines: make(chan []byte, 256), log: log, done: make(chan struct{})}
	cmd.Stderr = &p.errBuf
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", c.Path, err)
	}
	go func() {
		r := bufio.NewReaderSize(stdout, 1<<20)
		for {
			line, err := readLine(r)
			if len(line) > 0 {
				p.lines <- line
			}
			if err != nil {
				break
			}
		}
		close(p.lines)
	}()
	go func() {
		err := cmd.Wait()
		if err != nil {
			if msg := strings.TrimSpace(p.errBuf.String()); msg != "" {
				err = fmt.Errorf("%w: %s", err, lastLines(msg, 5))
			}
		}
		p.err = err
		close(p.done)
	}()
	go func() {
		select {
		case <-ctx.Done():
			p.stop()
		case <-p.done:
		}
	}()
	return p, nil
}

// readLine reads one line of any length up to maxLine.
func readLine(r *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if len(out)+len(chunk) <= maxLine {
			out = append(out, chunk...)
		}
		if err != nil {
			return out, err
		}
		if !isPrefix {
			return out, nil
		}
	}
}

// send writes v as one JSON line.
func (p *proc) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_, err = p.stdin.Write(append(b, '\n'))
	return err
}

// stop ends the process: SIGTERM to its group, SIGKILL after five seconds.
func (p *proc) stop() {
	if p.cmd.Process == nil {
		return
	}
	_ = p.stdin.Close()
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	}
}

func (p *proc) wait() error {
	<-p.done
	return p.err
}

// tail keeps the last 16 KiB written to it (a CLI's stderr).
type tail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tail) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if len(t.buf) > 16<<10 {
		t.buf = t.buf[len(t.buf)-16<<10:]
	}
	return len(b), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " / ")
}

// ErrNoSteer is returned by Steer when the CLI cannot take a message
// during a turn.
var ErrNoSteer = errors.New("the agent cannot take a message during a turn")

// Backgrounder is a driver whose CLI can move running foreground commands
// to the background (Claude Code's Ctrl+B), so the turn goes on.
type Backgrounder interface {
	// Background moves the running foreground commands to the background.
	Background() error
}

// ErrNoBackground is returned when the agent cannot move commands to the
// background.
var ErrNoBackground = errors.New("this agent cannot move commands to the background")

// ErrBusy is returned by Send while a turn runs.
var ErrBusy = errors.New("a turn is running")

// ErrNotRunning is returned when the CLI is not running.
var ErrNotRunning = errors.New("the agent CLI is not running")

// envWith returns env with key set to value.
func envWith(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return append(out, key+"="+value)
}

// fileExists reports whether path is a regular file.
func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}
