package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dseif0x/agents-operator/internal/auth"
	"github.com/dseif0x/agents-operator/internal/config"
	"github.com/dseif0x/agents-operator/internal/session"
	"github.com/dseif0x/agents-operator/internal/store"
	"github.com/dseif0x/agents-operator/internal/term"
)

type fakeOrch struct{}

func (fakeOrch) Notify(string) {}
func (fakeOrch) PodLogs(context.Context, string, int64) ([]byte, error) {
	return []byte("line1\n"), nil
}

type noResolver struct{}

func (noResolver) Endpoint(context.Context, string) (term.Endpoint, error) {
	return term.Endpoint{}, term.ErrNotRunning
}

type env struct {
	srv    *httptest.Server
	store  *store.Memory
	client *http.Client
	csrf   string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st := store.NewMemory()
	hash, _ := auth.HashPassword("pw")
	_ = st.Users().Create(context.Background(), &store.User{Username: "admin", PasswordHash: hash})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pub, _ := url.Parse("http://hub.test")
	cfg := &config.Config{PublicURL: pub, AllowedHosts: []string{"hub.test", "127.0.0.1"}, CookieSecret: bytes.Repeat([]byte("k"), 32)}
	creds := &session.Credentials{Store: st, CS: fake.NewClientset(), Namespace: "agents-operator"}
	proxy := &term.Proxy{Resolver: noResolver{}, Log: log}
	svc := &session.Service{Store: st, Orch: fakeOrch{}, Term: proxy, Broker: session.NewBroker(), Creds: creds,
		Defaults: session.Defaults{PVCSize: "20Gi"}, Log: log}
	s := &Server{
		Cfg: cfg, Store: st, Sessions: svc, Creds: creds,
		Auth:    auth.PasswordAuthenticator{Users: st.Users()},
		Cookies: auth.NewSessions(cfg.CookieSecret, false, st.Users()),
		Limiter: auth.NewRateLimiter(3, time.Minute),
		Term:    proxy, Ready: func() bool { return true }, Log: log,
		UI: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>ui</html>")) }),
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	jar := &cookieJar{}
	return &env{srv: srv, store: st, client: &http.Client{Jar: jar}}
}

// cookieJar is a minimal jar that keeps every cookie for every URL.
type cookieJar struct{ cookies []*http.Cookie }

func (j *cookieJar) SetCookies(_ *url.URL, cs []*http.Cookie) {
	for _, c := range cs {
		replaced := false
		for i, old := range j.cookies {
			if old.Name == c.Name {
				j.cookies[i] = c
				replaced = true
			}
		}
		if !replaced {
			j.cookies = append(j.cookies, c)
		}
	}
}

func (j *cookieJar) Cookies(*url.URL) []*http.Cookie {
	var out []*http.Cookie
	for _, c := range j.cookies {
		if c.MaxAge >= 0 && c.Value != "" {
			out = append(out, c)
		}
	}
	return out
}

func (e *env) do(t *testing.T, method, path string, body any, host string) (*http.Response, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	req.Host = host
	if e.csrf != "" {
		req.Header.Set(auth.CSRFHeader, e.csrf)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if out == nil {
		out = map[string]any{"_raw": string(raw)}
	}
	return resp, out
}

func (e *env) login(t *testing.T) {
	t.Helper()
	resp, body := e.do(t, http.MethodPost, "/api/v1/auth/login", map[string]string{"username": "admin", "password": "pw"}, "hub.test")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d %v", resp.StatusCode, body)
	}
	e.csrf, _ = body["csrf"].(string)
	if e.csrf == "" {
		t.Fatal("no csrf token")
	}
}

func TestHostAllowlist(t *testing.T) {
	e := newEnv(t)
	resp, _ := e.do(t, http.MethodGet, "/api/v1/auth/me", nil, "evil.test")
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("bad host = %d", resp.StatusCode)
	}
	resp, _ = e.do(t, http.MethodGet, "/healthz", nil, "evil.test")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz should ignore host: %d", resp.StatusCode)
	}
	resp, _ = e.do(t, http.MethodGet, "/", nil, "hub.test:443")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("host with port = %d", resp.StatusCode)
	}
}

func TestAuthAndCSRF(t *testing.T) {
	e := newEnv(t)
	resp, _ := e.do(t, http.MethodGet, "/api/v1/sessions", nil, "hub.test")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d", resp.StatusCode)
	}
	resp, _ = e.do(t, http.MethodPost, "/api/v1/auth/login", map[string]string{"username": "admin", "password": "nope"}, "hub.test")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad password = %d", resp.StatusCode)
	}
	e.login(t)
	resp, body := e.do(t, http.MethodGet, "/api/v1/auth/me", nil, "hub.test")
	if resp.StatusCode != http.StatusOK || body["user"].(map[string]any)["username"] != "admin" {
		t.Fatalf("me = %d %v", resp.StatusCode, body)
	}
	// Missing CSRF header on a POST is rejected.
	saved := e.csrf
	e.csrf = ""
	resp, _ = e.do(t, http.MethodPost, "/api/v1/sessions", map[string]string{"name": "x", "agent": "shell"}, "hub.test")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no csrf = %d", resp.StatusCode)
	}
	e.csrf = saved
	resp, _ = e.do(t, http.MethodPost, "/api/v1/auth/logout", nil, "hub.test")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logout = %d", resp.StatusCode)
	}
	resp, _ = e.do(t, http.MethodGet, "/api/v1/auth/me", nil, "hub.test")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after logout = %d", resp.StatusCode)
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 3; i++ {
		e.do(t, http.MethodPost, "/api/v1/auth/login", map[string]string{"username": "admin", "password": "nope"}, "hub.test")
	}
	resp, _ := e.do(t, http.MethodPost, "/api/v1/auth/login", map[string]string{"username": "admin", "password": "pw"}, "hub.test")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("locked out = %d", resp.StatusCode)
	}
}

func TestSessionCRUD(t *testing.T) {
	e := newEnv(t)
	e.login(t)
	resp, body := e.do(t, http.MethodPost, "/api/v1/sessions", map[string]any{"name": "one", "agent": "claude", "repo_url": "https://github.com/x/y.git", "bogus": 1}, "hub.test")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field = %d %v", resp.StatusCode, body)
	}
	resp, body = e.do(t, http.MethodPost, "/api/v1/sessions", map[string]any{"name": "one", "agent": "claude", "repo_url": "https://github.com/x/y.git"}, "hub.test")
	if resp.StatusCode != http.StatusCreated || body["state"] != "creating" {
		t.Fatalf("create = %d %v", resp.StatusCode, body)
	}
	id := body["id"].(string)

	resp, body = e.do(t, http.MethodGet, "/api/v1/sessions", nil, "hub.test")
	if resp.StatusCode != http.StatusOK || len(body["sessions"].([]any)) != 1 {
		t.Fatalf("list = %d %v", resp.StatusCode, body)
	}
	resp, body = e.do(t, http.MethodGet, "/api/v1/sessions/"+id, nil, "hub.test")
	if resp.StatusCode != http.StatusOK || body["pod_name"] != "agents-operator-"+id {
		t.Fatalf("get = %d %v", resp.StatusCode, body)
	}
	resp, _ = e.do(t, http.MethodGet, "/api/v1/sessions/"+store.NewID(), nil, "hub.test")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing = %d", resp.StatusCode)
	}
	// Terminal on a non-running session is a conflict.
	resp, _ = e.do(t, http.MethodGet, "/api/v1/sessions/"+id+"/ws", nil, "hub.test")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("ws on creating = %d", resp.StatusCode)
	}
	resp, _ = e.do(t, http.MethodGet, "/api/v1/sessions/"+id+"/scrollback", nil, "hub.test")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("scrollback on creating = %d", resp.StatusCode)
	}
	resp, _ = e.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/start", nil, "hub.test")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("start while creating = %d", resp.StatusCode)
	}
	resp, _ = e.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/stop", nil, "hub.test")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("stop = %d", resp.StatusCode)
	}
	resp, body = e.do(t, http.MethodGet, "/api/v1/sessions/"+id+"/events", nil, "hub.test")
	if resp.StatusCode != http.StatusOK || len(body["events"].([]any)) < 2 {
		t.Fatalf("events = %d %v", resp.StatusCode, body)
	}
	resp, body = e.do(t, http.MethodGet, "/api/v1/sessions/"+id+"/logs", nil, "hub.test")
	if resp.StatusCode != http.StatusOK || body["_raw"] != "line1\n" {
		t.Fatalf("logs = %d %v", resp.StatusCode, body)
	}
	resp, body = e.do(t, http.MethodDelete, "/api/v1/sessions/"+id, nil, "hub.test")
	if resp.StatusCode != http.StatusAccepted || body["state"] != "deleting" {
		t.Fatalf("delete = %d %v", resp.StatusCode, body)
	}
}

func TestCredentialsAPI(t *testing.T) {
	e := newEnv(t)
	e.login(t)
	resp, body := e.do(t, http.MethodPut, "/api/v1/me/credentials", map[string]any{"values": map[string]string{"anthropic_api_key": "sk", "git_user_email": "a@b.c", "openai_api_key": ""}}, "hub.test")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put = %d %v", resp.StatusCode, body)
	}
	creds := body["credentials"].([]any)
	if len(creds) != 2 {
		t.Fatalf("credentials = %v", creds)
	}
	for _, c := range creds {
		m := c.(map[string]any)
		if m["kind"] == "anthropic_api_key" && (m["secret"] != true || m["value"] != nil) {
			t.Fatalf("secret leaked: %v", m)
		}
		if m["kind"] == "git_user_email" && m["value"] != "a@b.c" {
			t.Fatalf("plain value missing: %v", m)
		}
	}
	resp, _ = e.do(t, http.MethodPut, "/api/v1/me/credentials", map[string]any{"values": map[string]string{"bogus": "x"}}, "hub.test")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bogus kind = %d", resp.StatusCode)
	}
	resp, _ = e.do(t, http.MethodDelete, "/api/v1/me/credentials/anthropic_api_key", nil, "hub.test")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	resp, body = e.do(t, http.MethodGet, "/api/v1/me/credentials", nil, "hub.test")
	if resp.StatusCode != http.StatusOK || len(body["credentials"].([]any)) != 1 {
		t.Fatalf("list = %d %v", resp.StatusCode, body)
	}
}

func TestSSE(t *testing.T) {
	e := newEnv(t)
	e.login(t)
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/sessions/events", nil)
	req.Host = "hub.test"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("sse = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	// Give the subscription a moment, then create a session and expect an event.
	time.Sleep(100 * time.Millisecond)
	go func() {
		b, _ := json.Marshal(map[string]any{"name": "sse", "agent": "shell"})
		req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/api/v1/sessions", bytes.NewReader(b))
		req.Host = "hub.test"
		req.Header.Set(auth.CSRFHeader, e.csrf)
		if resp, err := e.client.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	buf := make([]byte, 4096)
	var got string
	for !strings.Contains(got, "event: session") {
		n, err := resp.Body.Read(buf)
		if err != nil {
			t.Fatalf("read sse: %v (got %q)", err, got)
		}
		got += string(buf[:n])
	}
	if !strings.Contains(got, `"state":"creating"`) {
		t.Fatalf("sse payload = %q", got)
	}
}

func TestUIFallbackAndUnknownAPI(t *testing.T) {
	e := newEnv(t)
	resp, body := e.do(t, http.MethodGet, "/sessions/abc", nil, "hub.test")
	if resp.StatusCode != http.StatusOK || body["_raw"] != "<html>ui</html>" {
		t.Fatalf("ui = %d %v", resp.StatusCode, body)
	}
	resp, _ = e.do(t, http.MethodGet, "/api/v2/x", nil, "hub.test")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown api = %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("security headers missing")
	}
}
