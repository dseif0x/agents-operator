package main

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/dseif0x/agents-operator/internal/runner"
)

func TestSplitPrivateEnv(t *testing.T) {
	clean, private := splitPrivateEnv([]string{
		"RUNNER_TOKEN=tok", "GIT_SSH_KEY=-----BEGIN\nx", "AGENTS_OPERATOR_LOGIN_CLAUDE_LOGIN=e30=",
		"GH_TOKEN=ghp_x", "ANTHROPIC_API_KEY=sk", "PATH=/bin", "EMPTY=",
	})
	if !slices.Equal(clean, []string{"GH_TOKEN=ghp_x", "ANTHROPIC_API_KEY=sk", "PATH=/bin", "EMPTY="}) {
		t.Fatalf("clean = %q", clean)
	}
	if len(private) != 3 || private["RUNNER_TOKEN"] != "tok" || private["GIT_SSH_KEY"] != "-----BEGIN\nx" || private["AGENTS_OPERATOR_LOGIN_CLAUDE_LOGIN"] != "e30=" {
		t.Fatalf("private = %q", private)
	}
}

func TestPrivateFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "agents-operator-private-old")
	if err := os.WriteFile(stale, []byte(`{"RUNNER_TOKEN":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := writePrivateFile(dir, map[string]string{"RUNNER_TOKEN": "tok", "GIT_SSH_KEY": "key", "PATH": "/evil"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale hand-over file left behind")
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode: %v %v", st, err)
	}

	t.Setenv(runner.EnvPrivateFile, path)
	t.Setenv("RUNNER_TOKEN", "")
	t.Setenv("GIT_SSH_KEY", "")
	t.Setenv("PATH", os.Getenv("PATH"))
	if err := loadPrivateEnv(); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("RUNNER_TOKEN") != "tok" || os.Getenv("GIT_SSH_KEY") != "key" {
		t.Fatalf("not restored: %q %q", os.Getenv("RUNNER_TOKEN"), os.Getenv("GIT_SSH_KEY"))
	}
	if os.Getenv("PATH") == "/evil" {
		t.Fatal("a variable that is not runner-private was taken from the file")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("hand-over file not deleted")
	}
	if _, ok := os.LookupEnv(runner.EnvPrivateFile); ok {
		t.Fatal("file variable left in the environment")
	}

	// Nothing handed over: nothing to do.
	if err := loadPrivateEnv(); err != nil {
		t.Fatal(err)
	}
	// A file that is gone fails loudly instead of starting without a token.
	t.Setenv(runner.EnvPrivateFile, filepath.Join(dir, "missing"))
	if err := loadPrivateEnv(); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestEnvDropsRunnerPrivate(t *testing.T) {
	t.Setenv("RUNNER_TOKEN", "tok")
	t.Setenv("GIT_SSH_KEY", "key")
	t.Setenv("AGENTS_OPERATOR_LOGIN_CODEX_LOGIN", "e30=")
	ws := &Workspace{Root: t.TempDir()}
	for _, kv := range ws.Env() {
		if k, _, _ := strings.Cut(kv, "="); runnerPrivate(k) {
			t.Fatalf("agent env carries %s", k)
		}
	}
}

func TestHideProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	if err := hideProcess(); err != nil {
		t.Fatal(err)
	}
	// PR_GET_DUMPABLE returns the flag as the syscall result.
	if v, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_GET_DUMPABLE, 0, 0); errno != 0 || v != 0 {
		t.Fatalf("dumpable = %d (%v)", v, errno)
	}
}
