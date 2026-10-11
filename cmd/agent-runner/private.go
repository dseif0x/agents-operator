package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/dseif0x/agents-operator/internal/runner"
)

// The agent runs as the same user as the runner and everything else in the
// pod, so whatever sits in another process's environment or memory there is
// the agent's to read through /proc, unless that process is non-dumpable.
// The runner keeps a few values from the agent (its own bearer token, the
// SSH key it writes to disk, the saved logins it seeds), and two steps keep
// them out of reach:
//
//   - `agent-runner init <command>` is the container entrypoint. Before
//     anything else runs it moves those values out of the environment into
//     a fresh 0600 file under /tmp and execs <command> (tini, then the
//     runner) without them, so PID 1 never carries them. tini cannot be made
//     non-dumpable, which is why the environment it starts with has to be clean.
//   - The runner reads that file and deletes it before it starts the agent,
//     then marks itself non-dumpable (hideProcess), which closes
//     /proc/<pid>/environ, /proc/<pid>/mem and ptrace to processes of the
//     same user that hold no capabilities. The agent CLI is exec'd and
//     dumpable again, as normal.

// privateFilePattern names the hand-over file; init clears leftovers of it.
const privateFilePattern = "agents-operator-private-*"

// runnerPrivate reports whether an environment variable is for the runner
// alone and must never reach the agent.
func runnerPrivate(key string) bool {
	return key == runner.EnvRunnerToken || key == runner.EnvGitSSHKey || strings.HasPrefix(key, runner.EnvSeedPrefix)
}

// splitPrivateEnv separates the runner-only variables from the rest.
func splitPrivateEnv(environ []string) (clean []string, private map[string]string) {
	private = map[string]string{}
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		if runnerPrivate(k) {
			private[k] = v
			continue
		}
		clean = append(clean, kv)
	}
	return clean, private
}

// writePrivateFile stores private in a new file only this user can read and
// returns its path. The name is random and the file created exclusively, so
// nothing planted in /tmp beforehand can redirect the write.
func writePrivateFile(dir string, private map[string]string) (string, error) {
	// A file left by a runner that died before reading it holds stale values.
	if old, err := filepath.Glob(filepath.Join(dir, privateFilePattern)); err == nil {
		for _, p := range old {
			_ = os.Remove(p)
		}
	}
	f, err := os.CreateTemp(dir, privateFilePattern)
	if err != nil {
		return "", err
	}
	err = json.NewEncoder(f).Encode(private)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// initMain is `agent-runner init <command> [args...]`: move the runner-only
// variables into a file and exec command with the rest of the environment.
func initMain(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: agent-runner init <command> [args...]")
		return 2
	}
	path, err := exec.LookPath(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent-runner init:", err)
		return 1
	}
	env, private := splitPrivateEnv(os.Environ())
	if len(private) > 0 {
		file, err := writePrivateFile(os.TempDir(), private)
		if err != nil {
			fmt.Fprintln(os.Stderr, "agent-runner init: cannot hand over private environment:", err)
			return 1
		}
		env = append(env, runner.EnvPrivateFile+"="+file)
	}
	err = syscall.Exec(path, args, env)
	fmt.Fprintln(os.Stderr, "agent-runner init: exec", path+":", err)
	return 1
}

// loadPrivateEnv takes the variables init handed over back into this
// process's environment (Go's copy, not the one /proc shows) and deletes the
// file. Without init (tests, local runs) there is nothing to do.
func loadPrivateEnv() error {
	path := os.Getenv(runner.EnvPrivateFile)
	if path == "" {
		return nil
	}
	_ = os.Unsetenv(runner.EnvPrivateFile)
	raw, err := os.ReadFile(path)
	_ = os.Remove(path)
	if err != nil {
		return fmt.Errorf("read private environment: %w", err)
	}
	var private map[string]string
	if err := json.Unmarshal(raw, &private); err != nil {
		return fmt.Errorf("decode private environment: %w", err)
	}
	for k, v := range private {
		if !runnerPrivate(k) {
			continue
		}
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}
	return nil
}
