// Package term is the hub's side of the terminal: a dumb authenticated pipe
// between the browser WebSocket and the runner WebSocket, plus small HTTP
// helpers for status and scrollback. It never buffers or parses PTY bytes.
package term

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"github.com/dseif0x/agents-operator/internal/runner"
)

// Endpoint is where a session's runner listens and how to authenticate.
type Endpoint struct {
	IP    string
	Port  int
	Token string
}

// Resolver finds the runner endpoint for a session.
type Resolver interface {
	Endpoint(ctx context.Context, sessionID string) (Endpoint, error)
}

// ErrNotRunning is returned when the session has no reachable pod.
var ErrNotRunning = errors.New("session is not running")

// Proxy pipes WebSocket frames between browsers and runners.
type Proxy struct {
	Resolver Resolver
	// OriginPatterns are the hosts browsers may connect from.
	OriginPatterns []string
	Log            *slog.Logger
	// Client is used for status and scrollback calls.
	Client *http.Client
}

func (p *Proxy) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
	}}
}

func (e Endpoint) base() string { return "http://" + net.JoinHostPort(e.IP, strconv.Itoa(e.Port)) }

func (e Endpoint) header() http.Header {
	return http.Header{"Authorization": {"Bearer " + e.Token}}
}

// browserControlAllowed lists the control messages a browser may send. The
// hub-only export_login message is filtered so a browser cannot ask the
// runner for a credential file.
var browserControlAllowed = map[string]bool{runner.MsgResize: true, runner.MsgRestart: true}

// ServeWS upgrades the browser request and pipes it to the runner. onAttach
// is called once the runner connection is established.
func (p *Proxy) ServeWS(w http.ResponseWriter, r *http.Request, sessionID string, onAttach func()) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	ep, err := p.Resolver.Endpoint(ctx, sessionID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	dctx, dcancel := context.WithTimeout(ctx, 10*time.Second)
	rc, _, err := websocket.Dial(dctx, "ws://"+net.JoinHostPort(ep.IP, strconv.Itoa(ep.Port))+"/ws", &websocket.DialOptions{ //nolint:bodyclose // handshake response body is owned by coder/websocket
		HTTPHeader:      ep.header(),
		CompressionMode: websocket.CompressionDisabled,
	})
	dcancel()
	if err != nil {
		p.Log.Warn("dial runner failed", "session", sessionID, "err", err)
		http.Error(w, "runner unreachable", http.StatusBadGateway)
		return
	}
	// The runner's scrollback arrives in one frame of up to 2 MiB.
	rc.SetReadLimit(runner.ScrollbackSize + 64*1024)
	defer rc.Close(websocket.StatusNormalClosure, "bye")

	bc, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns:  p.OriginPatterns,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		p.Log.Warn("browser websocket accept failed", "err", err)
		return
	}
	bc.SetReadLimit(1 << 20)
	defer bc.CloseNow()
	if onAttach != nil {
		onAttach()
	}

	errc := make(chan error, 3)
	// browser -> runner
	go func() {
		for {
			typ, data, err := bc.Read(ctx)
			if err != nil {
				errc <- fmt.Errorf("browser: %w", err)
				return
			}
			if typ == websocket.MessageText {
				var m runner.Control
				if json.Unmarshal(data, &m) != nil || !browserControlAllowed[m.T] {
					continue
				}
			}
			if err := writeWithDeadline(ctx, rc, typ, data); err != nil {
				errc <- fmt.Errorf("runner: %w", err)
				return
			}
		}
	}()
	// runner -> browser
	go func() {
		for {
			typ, data, err := rc.Read(ctx)
			if err != nil {
				errc <- fmt.Errorf("runner: %w", err)
				return
			}
			if err := writeWithDeadline(ctx, bc, typ, data); err != nil {
				errc <- fmt.Errorf("browser: %w", err)
				return
			}
		}
	}()
	// keepalive to the browser
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
				err := bc.Ping(pctx)
				pcancel()
				if err != nil {
					misses++
					if misses >= runner.PingMisses {
						errc <- errors.New("browser missed pings")
						return
					}
				} else {
					misses = 0
				}
			}
		}
	}()

	err = <-errc
	var ce websocket.CloseError
	switch {
	case errors.As(err, &ce) && (ce.Code == websocket.StatusNormalClosure || ce.Code == websocket.StatusGoingAway):
		// Browser closed the tab, or the runner shut down cleanly.
	case errors.Is(err, context.Canceled):
	default:
		p.Log.Debug("terminal pipe ended", "session", sessionID, "err", err)
	}
	// Tell the browser why, so it can decide whether to reconnect.
	_ = bc.Close(websocket.StatusPolicyViolation, "runner connection closed")
}

func writeWithDeadline(ctx context.Context, c *websocket.Conn, typ websocket.MessageType, data []byte) error {
	wctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return c.Write(wctx, typ, data)
}

// Status fetches the runner's status document.
func (p *Proxy) Status(ctx context.Context, sessionID string) (runner.Status, error) {
	var st runner.Status
	ep, err := p.Resolver.Endpoint(ctx, sessionID)
	if err != nil {
		return st, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.base()+"/status", nil)
	if err != nil {
		return st, err
	}
	req.Header = ep.header()
	resp, err := p.client().Do(req)
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return st, fmt.Errorf("runner status: http %d", resp.StatusCode)
	}
	return st, json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st)
}

// Scrollback fetches the runner's raw scrollback buffer.
func (p *Proxy) Scrollback(ctx context.Context, sessionID string) ([]byte, error) {
	ep, err := p.Resolver.Endpoint(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.base()+"/scrollback", nil)
	if err != nil {
		return nil, err
	}
	req.Header = ep.header()
	resp, err := p.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("runner scrollback: http %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, runner.ScrollbackSize+1024))
}

// Control opens a short-lived connection to the runner, sends one control
// message and waits for a reply of type wantReply (or an error message).
// It is how the hub asks for a restart or exports a saved login.
func (p *Proxy) Control(ctx context.Context, sessionID string, msg runner.Control, wantReply string) (runner.Control, error) {
	var reply runner.Control
	ep, err := p.Resolver.Endpoint(ctx, sessionID)
	if err != nil {
		return reply, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws://"+net.JoinHostPort(ep.IP, strconv.Itoa(ep.Port))+"/ws", &websocket.DialOptions{HTTPHeader: ep.header()}) //nolint:bodyclose // handshake response body is owned by coder/websocket
	if err != nil {
		return reply, fmt.Errorf("dial runner: %w", err)
	}
	c.SetReadLimit(runner.ScrollbackSize + 64*1024)
	defer c.CloseNow()
	b, err := json.Marshal(msg)
	if err != nil {
		return reply, err
	}
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		return reply, err
	}
	if wantReply == "" {
		_ = c.Close(websocket.StatusNormalClosure, "")
		return reply, nil
	}
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return reply, err
		}
		if typ != websocket.MessageText {
			continue
		}
		if err := json.Unmarshal(data, &reply); err != nil {
			continue
		}
		switch reply.T {
		case wantReply:
			_ = c.Close(websocket.StatusNormalClosure, "")
			return reply, nil
		case runner.MsgError:
			return reply, errors.New(reply.Message)
		}
	}
}
