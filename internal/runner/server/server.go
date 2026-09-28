// Package server implements the agent-runner: a PTY owner that exposes the
// agent's terminal over a WebSocket with scrollback replay.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/dseif0x/agents-operator/internal/runner"
)

// Server serves the runner HTTP API: /ws, /healthz, /status, /scrollback.
type Server struct {
	Proc  *Process
	Token string
	Agent string
	Home  string
	Log   *slog.Logger

	clients atomic.Int64
}

// Handler returns the HTTP handler for the runner.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /status", s.auth(s.status))
	mux.HandleFunc("GET /scrollback", s.auth(s.scrollback))
	mux.HandleFunc("GET /ws", s.auth(s.ws))
	return mux
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if s.Token == "" || !strings.HasPrefix(h, prefix) ||
			subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(h, prefix)), []byte(s.Token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	st := s.Proc.Status(s.Agent, int(s.clients.Load()))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}

func (s *Server) scrollback(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(s.Proc.Scrollback())
}

func (s *Server) ws(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The hub is the only caller and it authenticates with the bearer
		// token; the browser never reaches this endpoint directly.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		s.Log.Warn("websocket accept failed", "err", err)
		return
	}
	c.SetReadLimit(1 << 20)
	s.clients.Add(1)
	defer s.clients.Add(-1)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer c.Close(websocket.StatusNormalClosure, "bye")

	sub := s.Proc.Subscribe()
	defer sub.Close()

	cols, rows := s.Proc.Size()
	if err := writeControl(ctx, c, runner.Control{T: runner.MsgHello, Scrollback: len(sub.Scrollback) > 0, Cols: cols, Rows: rows}); err != nil {
		return
	}
	if len(sub.Scrollback) > 0 {
		if err := c.Write(ctx, websocket.MessageBinary, sub.Scrollback); err != nil {
			return
		}
	}
	// If the agent already exited, tell the new client at once.
	if sub.Exited != nil {
		_ = writeControl(ctx, c, runner.Control{T: runner.MsgExit, Code: sub.Exited})
	}

	errc := make(chan error, 3)

	// Output pump: PTY -> client, plus exit notifications.
	go func() {
		for {
			select {
			case <-ctx.Done():
				errc <- ctx.Err()
				return
			case chunk := <-sub.Out:
				if err := c.Write(ctx, websocket.MessageBinary, chunk); err != nil {
					errc <- err
					return
				}
			case code := <-sub.Exit:
				if err := writeControl(ctx, c, runner.Control{T: runner.MsgExit, Code: &code}); err != nil {
					errc <- err
					return
				}
			}
		}
	}()

	// Ping loop: drop the client after three missed pongs.
	go func() {
		t := time.NewTicker(runner.PingInterval)
		defer t.Stop()
		misses := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, pcancel := context.WithTimeout(ctx, runner.PingInterval)
				err := c.Ping(pctx)
				pcancel()
				if err != nil {
					misses++
					if misses >= runner.PingMisses {
						errc <- errors.New("client missed pings")
						return
					}
				} else {
					misses = 0
				}
			}
		}
	}()

	// Input pump: client -> PTY and control messages.
	go func() {
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				errc <- err
				return
			}
			switch typ {
			case websocket.MessageBinary:
				if err := s.Proc.Write(data); err != nil {
					s.Log.Debug("write to pty failed", "err", err)
				}
			case websocket.MessageText:
				s.handleControl(ctx, c, data)
			}
		}
	}()

	err = <-errc
	if err != nil && !errors.Is(err, context.Canceled) {
		s.Log.Debug("client disconnected", "err", err)
	}
}

func (s *Server) handleControl(ctx context.Context, c *websocket.Conn, data []byte) {
	var msg runner.Control
	if err := json.Unmarshal(data, &msg); err != nil {
		_ = writeControl(ctx, c, runner.Control{T: runner.MsgError, Message: "bad control message"})
		return
	}
	switch msg.T {
	case runner.MsgResize:
		if err := s.Proc.Resize(msg.Cols, msg.Rows); err != nil {
			_ = writeControl(ctx, c, runner.Control{T: runner.MsgError, Message: err.Error()})
		}
	case runner.MsgRestart:
		s.Log.Info("restart requested")
		if err := s.Proc.Restart(ctx); err != nil {
			_ = writeControl(ctx, c, runner.Control{T: runner.MsgError, Message: err.Error()})
		}
	case runner.MsgExportLogin:
		files, ok := runner.LoginFiles[msg.Kind]
		if !ok {
			_ = writeControl(ctx, c, runner.Control{T: runner.MsgError, Message: "unknown login kind"})
			return
		}
		bundle := runner.LoginBundle{Files: map[string][]byte{}}
		for i, rel := range files {
			b, err := os.ReadFile(filepath.Join(s.Home, rel))
			if err != nil {
				if i == 0 {
					_ = writeControl(ctx, c, runner.Control{T: runner.MsgError, Message: "login file not found; run the CLI's login first"})
					return
				}
				continue // optional companion file
			}
			bundle.Files[rel] = b
		}
		enc, err := runner.EncodeLoginBundle(bundle)
		if err != nil {
			_ = writeControl(ctx, c, runner.Control{T: runner.MsgError, Message: err.Error()})
			return
		}
		_ = writeControl(ctx, c, runner.Control{T: runner.MsgLogin, Kind: msg.Kind, Data: base64.StdEncoding.EncodeToString(enc)})
	default:
		_ = writeControl(ctx, c, runner.Control{T: runner.MsgError, Message: "unknown control message"})
	}
}

func writeControl(ctx context.Context, c *websocket.Conn, msg runner.Control) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, b)
}
