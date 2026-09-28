// Package api serves the REST API, the WebSocket terminal route, the SSE
// feed, health and metrics, and hands everything else to the embedded UI.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/dseif0x/agents-operator/internal/auth"
	"github.com/dseif0x/agents-operator/internal/config"
	"github.com/dseif0x/agents-operator/internal/runner"
	"github.com/dseif0x/agents-operator/internal/session"
	"github.com/dseif0x/agents-operator/internal/store"
	"github.com/dseif0x/agents-operator/internal/term"
)

// Server holds the handler dependencies.
type Server struct {
	Cfg      *config.Config
	Store    store.Store
	Sessions *session.Service
	Creds    *session.Credentials
	Auth     auth.Authenticator
	Cookies  *auth.Sessions
	Limiter  *auth.RateLimiter
	Term     *term.Proxy
	Ready    func() bool
	UI       http.Handler
	Metrics  http.Handler
	Log      *slog.Logger
}

type ctxKey int

const principalKey ctxKey = 1

func principal(r *http.Request) *auth.Principal {
	p, _ := r.Context().Value(principalKey).(*auth.Principal)
	return p
}

// Handler builds the full router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeText(w, http.StatusOK, "ok") })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if s.Ready != nil && !s.Ready() {
			writeText(w, http.StatusServiceUnavailable, "not ready")
			return
		}
		writeText(w, http.StatusOK, "ok")
	})
	if s.Metrics != nil {
		mux.Handle("GET /metrics", s.Metrics)
	}

	// Public auth routes.
	mux.HandleFunc("POST /api/v1/auth/login", s.login)
	mux.HandleFunc("POST /api/v1/auth/logout", s.logout)

	// Authenticated routes.
	authed := http.NewServeMux()
	authed.HandleFunc("GET /api/v1/auth/me", s.me)
	authed.HandleFunc("GET /api/v1/sessions", s.listSessions)
	authed.HandleFunc("POST /api/v1/sessions", s.createSession)
	authed.HandleFunc("GET /api/v1/sessions/events", s.sessionEvents)
	authed.HandleFunc("GET /api/v1/sessions/{id}", s.getSession)
	authed.HandleFunc("DELETE /api/v1/sessions/{id}", s.deleteSession)
	authed.HandleFunc("POST /api/v1/sessions/{id}/start", s.startSession)
	authed.HandleFunc("POST /api/v1/sessions/{id}/stop", s.stopSession)
	authed.HandleFunc("POST /api/v1/sessions/{id}/restart-agent", s.restartAgent)
	authed.HandleFunc("POST /api/v1/sessions/{id}/save-login", s.saveLogin)
	authed.HandleFunc("GET /api/v1/sessions/{id}/ws", s.sessionWS)
	authed.HandleFunc("GET /api/v1/sessions/{id}/scrollback", s.scrollback)
	authed.HandleFunc("GET /api/v1/sessions/{id}/events", s.sessionEventLog)
	authed.HandleFunc("GET /api/v1/sessions/{id}/logs", s.sessionLogs)
	authed.HandleFunc("GET /api/v1/me/credentials", s.listCredentials)
	authed.HandleFunc("GET /api/v1/me/github/repos", s.githubRepos)
	authed.HandleFunc("PUT /api/v1/me/credentials", s.putCredentials)
	authed.HandleFunc("DELETE /api/v1/me/credentials/{kind}", s.deleteCredential)
	mux.Handle("/api/v1/", s.requireAuth(s.requireCSRF(authed)))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) { writeErr(w, http.StatusNotFound, "no such route") })

	if s.UI != nil {
		mux.Handle("/", s.UI)
	}
	return s.recover(s.hostAllowlist(s.logging(s.securityHeaders(mux))))
}

// ---- middleware ----

func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(rec) // net/http handles this one itself
			}
			s.Log.Error("panic", "err", rec, "path", r.URL.Path)
			writeErr(w, http.StatusInternalServerError, "internal error")
		}()
		next.ServeHTTP(w, r)
	})
}

// hostAllowlist rejects requests whose Host is not the public host, which
// blocks DNS rebinding. Health probes come from kubelet with a pod IP host
// and are exempt.
func (s *Server) hostAllowlist(next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, h := range s.Cfg.AllowedHosts {
		allowed[strings.ToLower(h)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		host := strings.ToLower(r.Host)
		if !allowed[host] {
			if h, _, ok := strings.Cut(host, ":"); !ok || !allowed[h] {
				s.Log.Warn("rejected host", "host", r.Host, "path", r.URL.Path)
				writeErr(w, http.StatusMisdirectedRequest, "host not allowed")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; connect-src 'self' ws: wss:; worker-src 'self' blob:; frame-ancestors 'none'")
		}
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the hijacker for WebSockets
// and the flusher for SSE.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		s.Log.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "ms", time.Since(start).Milliseconds(), "ip", auth.ClientIP(r))
	})
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.Cookies.Read(r)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "not logged in")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	})
}

func (s *Server) requireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if !s.Cookies.CheckCSRF(r, principal(r)) {
				writeErr(w, http.StatusForbidden, "missing or invalid CSRF token")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeText(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg+"\n")
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

// mapErr turns service errors into HTTP responses.
func (s *Server) mapErr(w http.ResponseWriter, err error) {
	var ve *session.ValidationError
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "session not found")
	case errors.Is(err, session.ErrInvalidTransition):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, term.ErrNotRunning):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.As(err, &ve):
		writeErr(w, http.StatusBadRequest, ve.Msg)
	default:
		s.Log.Error("request failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}

// ---- auth ----

type userJSON struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

type meJSON struct {
	User userJSON `json:"user"`
	CSRF string   `json:"csrf"`
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ip := auth.ClientIP(r)
	if !s.Limiter.Allowed(ip) {
		writeErr(w, http.StatusTooManyRequests, "too many failed logins; try again later")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	u, err := s.Auth.Login(r.Context(), body.Username, body.Password)
	if err != nil {
		if errors.Is(err, auth.ErrBadCredentials) {
			s.Limiter.Fail(ip)
			writeErr(w, http.StatusUnauthorized, err.Error())
			return
		}
		s.mapErr(w, err)
		return
	}
	s.Limiter.Reset(ip)
	p := s.Cookies.Issue(w, u)
	writeJSON(w, http.StatusOK, meJSON{User: userJSON{u.ID, u.Username}, CSRF: s.Cookies.CSRFToken(p)})
}

func (s *Server) logout(w http.ResponseWriter, _ *http.Request) {
	s.Cookies.Clear(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	writeJSON(w, http.StatusOK, meJSON{User: userJSON{p.User.ID, p.User.Username}, CSRF: s.Cookies.CSRFToken(p)})
}

// ---- sessions ----

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	list, err := s.Sessions.List(r.Context(), p.User.ID)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	views := make([]session.View, 0, len(list))
	for _, sess := range list {
		views = append(views, s.Sessions.View(sess))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": views, "agents": runner.Agents})
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var req session.CreateRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	sess, err := s.Sessions.Create(r.Context(), p.User, req)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.Sessions.View(sess))
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	sess, err := s.Sessions.Get(r.Context(), p.User.ID, r.PathValue("id"))
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Sessions.View(sess))
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	sess, err := s.Sessions.Delete(r.Context(), p.User, r.PathValue("id"))
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.Sessions.View(sess))
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	sess, err := s.Sessions.Start(r.Context(), p.User, r.PathValue("id"))
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.Sessions.View(sess))
}

func (s *Server) stopSession(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	sess, err := s.Sessions.Stop(r.Context(), p.User, r.PathValue("id"))
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.Sessions.View(sess))
}

func (s *Server) restartAgent(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := s.Sessions.RestartAgent(r.Context(), p.User, r.PathValue("id")); err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

func (s *Server) saveLogin(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var body struct {
		Kind string `json:"kind"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Sessions.SaveLogin(r.Context(), p.User, r.PathValue("id"), body.Kind); err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, session.ErrInvalidTransition) {
			s.mapErr(w, err)
			return
		}
		var ve *session.ValidationError
		if errors.As(err, &ve) {
			s.mapErr(w, err)
			return
		}
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) sessionWS(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := r.PathValue("id")
	sess, err := s.Sessions.Get(r.Context(), p.User.ID, id)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	if sess.State != store.StateRunning {
		writeErr(w, http.StatusConflict, "session is "+sess.State)
		return
	}
	s.Term.ServeWS(w, r, id, func() { s.Sessions.Attached(r.Context(), id) })
}

func (s *Server) scrollback(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := r.PathValue("id")
	if _, err := s.Sessions.Get(r.Context(), p.User.ID, id); err != nil {
		s.mapErr(w, err)
		return
	}
	b, err := s.Term.Scrollback(r.Context(), id)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

func (s *Server) sessionEventLog(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	evs, err := s.Sessions.Events(r.Context(), p.User.ID, r.PathValue("id"))
	if err != nil {
		s.mapErr(w, err)
		return
	}
	type ev struct {
		ID      int64     `json:"id"`
		At      time.Time `json:"at"`
		Kind    string    `json:"kind"`
		Message string    `json:"message"`
	}
	out := make([]ev, 0, len(evs))
	for _, e := range evs {
		out = append(out, ev{e.ID, e.At, e.Kind, e.Message})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

func (s *Server) sessionLogs(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	b, err := s.Sessions.Logs(r.Context(), p.User.ID, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.mapErr(w, err)
			return
		}
		writeErr(w, http.StatusBadGateway, "logs unavailable: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

// sessionEvents streams state changes for the caller's sessions as SSE.
func (s *Server) sessionEvents(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, ": connected\n\n")
	_ = rc.Flush()

	ch, cancel := s.Sessions.Broker.Subscribe(p.User.ID)
	defer cancel()
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			_ = rc.Flush()
		case ev := <-ch:
			b, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, b); err != nil {
				return
			}
			_ = rc.Flush()
		}
	}
}

// githubRepos lists repositories reachable with the user's GitHub token for
// the session form. {"configured": false} when no token is set.
func (s *Server) githubRepos(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	repos, configured, err := s.Sessions.GitHubRepos(r.Context(), p.User.ID, r.URL.Query().Get("refresh") == "1")
	if err != nil {
		var ve *session.ValidationError
		if errors.As(err, &ve) {
			writeJSON(w, http.StatusOK, map[string]any{"configured": configured, "repos": []any{}, "error": ve.Msg})
			return
		}
		writeErr(w, http.StatusBadGateway, "github: "+err.Error())
		return
	}
	if repos == nil {
		repos = []session.GitHubRepo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"configured": configured, "repos": repos})
}

// ---- credentials ----

func (s *Server) listCredentials(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	list, err := s.Creds.List(r.Context(), p.User.ID)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": list, "kinds": store.CredentialKinds})
}

// putCredentials accepts {"values": {"anthropic_api_key": "...", ...}}.
// Empty strings are ignored; use DELETE to remove a kind.
func (s *Server) putCredentials(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var body struct {
		Values map[string]string `json:"values"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	for kind, v := range body.Values {
		if strings.TrimSpace(v) == "" {
			continue
		}
		if err := s.Creds.Set(r.Context(), p.User.ID, kind, []byte(v)); err != nil {
			s.mapErr(w, err)
			return
		}
	}
	list, err := s.Creds.List(r.Context(), p.User.ID)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": list, "kinds": store.CredentialKinds})
}

func (s *Server) deleteCredential(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := s.Creds.Delete(r.Context(), p.User.ID, r.PathValue("kind")); err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
