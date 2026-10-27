package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/dseif0x/agents-operator/internal/runner"
)

// hookForward is `agent-runner hook`: the command Claude Code runs on each
// hook event. It reads the event JSON from stdin, posts it to the runner's
// loopback hook endpoint and exits 0 whatever happens, so a hook can never
// fail the CLI. An answer from the runner (a Stop blocked because chat
// messages are waiting) is written to stdout, where the CLI reads it.
func hookForward(url string, stdin io.Reader, stdout io.Writer) {
	data, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil || len(data) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		_, _ = io.Copy(stdout, io.LimitReader(resp.Body, 1<<20))
	}
}

func hookURL() string {
	if v := os.Getenv(runner.EnvHookURL); v != "" {
		return v
	}
	return runner.HookURL
}
