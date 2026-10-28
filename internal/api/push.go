package api

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/dseif0x/agents-operator/internal/push"
	"github.com/dseif0x/agents-operator/internal/store"
)

// ---- push notifications ----

func (s *Server) pushRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/push/key", s.pushKey)
	mux.HandleFunc("POST /api/v1/push/subscriptions", s.pushSubscribe)
	mux.HandleFunc("DELETE /api/v1/push/subscriptions", s.pushUnsubscribe)
	mux.HandleFunc("POST /api/v1/push/test", s.pushTest)
	mux.HandleFunc("POST /api/v1/presence", s.presence)
}

func (s *Server) pushKey(w http.ResponseWriter, r *http.Request) {
	if s.Push == nil {
		writeErr(w, http.StatusNotFound, "push notifications are off")
		return
	}
	key, err := s.Push.PublicKey(r.Context())
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"public_key": key})
}

// pushSubscribe stores a browser's PushSubscription (its toJSON shape).
func (s *Server) pushSubscribe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Endpoint       string  `json:"endpoint"`
		ExpirationTime *uint64 `json:"expirationTime"`
		Keys           struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Only https endpoints of a push service; the hub POSTs to them.
	u, err := url.Parse(body.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || len(body.Endpoint) > 2048 || body.Keys.P256dh == "" || body.Keys.Auth == "" {
		writeErr(w, http.StatusBadRequest, "not a push subscription")
		return
	}
	ua := r.Header.Get("User-Agent")
	if len(ua) > 300 {
		ua = ua[:300]
	}
	if err := s.Store.Push().Save(r.Context(), &store.PushSubscription{
		Endpoint: body.Endpoint, UserID: principal(r).User.ID, P256dh: body.Keys.P256dh, Auth: body.Keys.Auth, UserAgent: ua,
	}); err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) pushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Endpoint string `json:"endpoint"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Store.Push().Delete(r.Context(), principal(r).User.ID, body.Endpoint); err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// pushTest sends a test notification to every device of the user.
func (s *Server) pushTest(w http.ResponseWriter, r *http.Request) {
	if s.Push == nil {
		writeErr(w, http.StatusNotFound, "push notifications are off")
		return
	}
	n, err := s.Push.Send(r.Context(), principal(r).User.ID, push.Notification{
		Title: "agents-operator", Body: "Notifications work on this device.", URL: "/", Tag: "test",
	})
	if err != nil && n == 0 {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"sent": n})
}

// presence is the page the user looks at ("" when the app is hidden), so
// notifications for it are not sent.
func (s *Server) presence(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.Push != nil {
		p := body.Path
		if len(p) > 200 || !strings.HasPrefix(p, "/") {
			p = ""
		}
		s.Push.Present(principal(r).User.ID, p)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
