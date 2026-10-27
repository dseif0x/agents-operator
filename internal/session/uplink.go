package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/dseif0x/agents-operator/internal/runner"
	"github.com/dseif0x/agents-operator/internal/store"
	"github.com/dseif0x/agents-operator/internal/term"
)

// Uplinks keeps one uplink (runner.UplinkPath) to every running session
// that is in a chat: the channel its agent's chat tools talk to the hub
// through, and the one queued messages are pushed down. Only the leader
// holds uplinks; Ensure is called from its poll loop.
type Uplinks struct {
	Svc      *Service
	Resolver term.Resolver
	Log      *slog.Logger

	mu    sync.Mutex
	conns map[string]*uplinkConn
}

type uplinkConn struct {
	kick   chan struct{}
	cancel context.CancelFunc
}

// NewUplinks returns a manager for svc.
func NewUplinks(svc *Service, resolver term.Resolver, log *slog.Logger) *Uplinks {
	return &Uplinks{Svc: svc, Resolver: resolver, Log: log, conns: map[string]*uplinkConn{}}
}

// Notify asks the session's uplink to push what is queued. Without one,
// the next Ensure opens it (if the session runs and is in a chat).
func (u *Uplinks) Notify(sessionID string) {
	u.mu.Lock()
	c := u.conns[sessionID]
	u.mu.Unlock()
	if c != nil {
		select {
		case c.kick <- struct{}{}:
		default:
		}
	}
}

// Ensure opens uplinks to the running sessions in chats that have none,
// closes those of sessions no longer running, and has every open one push
// its queue (a safety net for notifications another hub replica got).
func (u *Uplinks) Ensure(ctx context.Context, running []*store.Session) {
	want := map[string]*store.Session{}
	for _, s := range running {
		if s.State == store.StateRunning && u.Svc.InChats(ctx, s.ID) {
			want[s.ID] = s
		}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	for id, c := range u.conns {
		if want[id] == nil {
			c.cancel()
			delete(u.conns, id)
		}
	}
	for id := range want {
		if c := u.conns[id]; c != nil {
			select {
			case c.kick <- struct{}{}:
			default:
			}
			continue
		}
		cctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		c := &uplinkConn{kick: make(chan struct{}, 1), cancel: cancel}
		u.conns[id] = c
		go u.run(cctx, id, c)
	}
}

// CloseAll drops every uplink (leadership lost, shutdown).
func (u *Uplinks) CloseAll() {
	u.mu.Lock()
	defer u.mu.Unlock()
	for id, c := range u.conns {
		c.cancel()
		delete(u.conns, id)
	}
}

func (u *Uplinks) forget(id string, c *uplinkConn) {
	u.mu.Lock()
	if u.conns[id] == c {
		delete(u.conns, id)
	}
	u.mu.Unlock()
	c.cancel()
}

// run holds one uplink until it fails or is cancelled; Ensure reopens it.
func (u *Uplinks) run(ctx context.Context, sessionID string, c *uplinkConn) {
	defer u.forget(sessionID, c)
	ep, err := u.Resolver.Endpoint(ctx, sessionID)
	if err != nil {
		return
	}
	dctx, dcancel := context.WithTimeout(ctx, 10*time.Second)
	ws, _, err := websocket.Dial(dctx, "ws://"+net.JoinHostPort(ep.IP, strconv.Itoa(ep.Port))+runner.UplinkPath, &websocket.DialOptions{ //nolint:bodyclose // owned by coder/websocket
		HTTPHeader:      map[string][]string{"Authorization": {"Bearer " + ep.Token}},
		CompressionMode: websocket.CompressionDisabled,
	})
	dcancel()
	if err != nil {
		// An older runner without /uplink, or one still starting.
		u.Log.Debug("uplink dial failed", "session", sessionID, "err", err)
		return
	}
	ws.SetReadLimit(runner.UplinkMaxFrame)
	defer ws.CloseNow()
	u.Log.Info("uplink open", "session", sessionID)

	var wmu sync.Mutex
	write := func(f runner.UplinkFrame) error {
		b, err := json.Marshal(f)
		if err != nil {
			return err
		}
		wmu.Lock()
		defer wmu.Unlock()
		wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return ws.Write(wctx, websocket.MessageText, b)
	}
	push := func() error {
		sess, err := u.Svc.Store.Sessions().Get(ctx, sessionID)
		if err != nil {
			return err
		}
		ds, err := u.Svc.QueuedDeliveries(ctx, sess)
		if err != nil || len(ds) == 0 {
			return err
		}
		var out []runner.ChatDelivery
		if err := convert(ds, &out); err != nil {
			return err
		}
		return write(runner.UplinkFrame{Event: runner.EventDeliver, Deliveries: out})
	}

	errc := make(chan error, 3)
	go func() {
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				errc <- err
				return
			}
			var f runner.UplinkFrame
			if json.Unmarshal(data, &f) != nil || f.Op == "" {
				continue
			}
			// Requests are short; one at a time keeps ordering simple.
			go func() {
				resp := u.handle(ctx, sessionID, f)
				if err := write(resp); err != nil {
					errc <- err
				}
			}()
		}
	}()
	go func() {
		t := time.NewTicker(runner.PingInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, cancel := context.WithTimeout(ctx, runner.PingInterval)
				err := ws.Ping(pctx)
				cancel()
				if err != nil {
					errc <- err
					return
				}
			}
		}
	}()
	if err := push(); err != nil {
		u.Log.Warn("uplink push failed", "session", sessionID, "err", err)
	}
	for {
		select {
		case <-ctx.Done():
			_ = ws.Close(websocket.StatusNormalClosure, "")
			return
		case err := <-errc:
			if !errors.Is(err, context.Canceled) {
				u.Log.Debug("uplink closed", "session", sessionID, "err", err)
			}
			return
		case <-c.kick:
			if err := push(); err != nil {
				u.Log.Warn("uplink push failed", "session", sessionID, "err", err)
			}
		}
	}
}

// convert copies between the API and wire shapes, which share their JSON.
func convert(in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// handle answers one runner request on behalf of the session's agent.
func (u *Uplinks) handle(ctx context.Context, sessionID string, f runner.UplinkFrame) runner.UplinkFrame {
	resp := runner.UplinkFrame{ID: f.ID}
	result, err := u.do(ctx, sessionID, f)
	if err != nil {
		var ve *ValidationError
		switch {
		case errors.As(err, &ve):
			resp.Error = ve.Msg
		case errors.Is(err, store.ErrNotFound):
			resp.Error = "not found"
		default:
			u.Log.Warn("uplink request failed", "session", sessionID, "op", f.Op, "err", err)
			resp.Error = "the hub could not do that: internal error"
		}
		return resp
	}
	b, err := json.Marshal(result)
	if err != nil {
		resp.Error = err.Error()
		return resp
	}
	resp.OK, resp.Result = true, b
	return resp
}

func (u *Uplinks) do(ctx context.Context, sessionID string, f runner.UplinkFrame) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	sess, err := u.Svc.Store.Sessions().Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	switch f.Op {
	case runner.OpChatsList:
		return u.Svc.AgentChats(ctx, sess)
	case runner.OpChatInfo:
		var a runner.ChatRefArgs
		if err := json.Unmarshal(f.Args, &a); err != nil {
			return nil, &ValidationError{"bad arguments"}
		}
		return u.Svc.AgentChatInfo(ctx, sess, a.Chat)
	case runner.OpChatRead:
		var a runner.ChatRefArgs
		if err := json.Unmarshal(f.Args, &a); err != nil {
			return nil, &ValidationError{"bad arguments"}
		}
		chat, msgs, err := u.Svc.AgentReadChat(ctx, sess, a.Chat, a.Since, a.Limit)
		if err != nil {
			return nil, err
		}
		return map[string]any{"chat": chat, "messages": msgs}, nil
	case runner.OpChatSend:
		var a runner.ChatSendArgs
		if err := json.Unmarshal(f.Args, &a); err != nil {
			return nil, &ValidationError{"bad arguments"}
		}
		images := make([]ChatImage, 0, len(a.Images))
		for _, img := range a.Images {
			images = append(images, ChatImage{Name: img.Name, Data: img.Data})
		}
		return u.Svc.AgentPost(ctx, sess, a.Chat, a.Text, a.ReplyTo, images)
	case runner.OpAttachment:
		var a struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(f.Args, &a); err != nil {
			return nil, &ValidationError{"bad arguments"}
		}
		att, data, err := u.Svc.AgentAttachment(ctx, sess, a.ID)
		if err != nil {
			return nil, err
		}
		return runner.UplinkFile{Name: att.Name, ContentType: att.ContentType, Data: data}, nil
	case runner.OpDelivered:
		var a struct {
			IDs []int64 `json:"ids"`
		}
		if err := json.Unmarshal(f.Args, &a); err != nil {
			return nil, &ValidationError{"bad arguments"}
		}
		return map[string]bool{"ok": true}, u.Svc.MarkDelivered(ctx, sess, a.IDs)
	default:
		return nil, &ValidationError{fmt.Sprintf("unknown op %q", f.Op)}
	}
}
