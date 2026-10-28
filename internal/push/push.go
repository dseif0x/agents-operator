// Package push sends Web Push notifications to the browsers and phones a
// user allowed them on: chat messages from agents and sessions that need
// attention. The VAPID key pair is generated once and kept in the database,
// so subscriptions survive hub restarts and replicas agree on it.
//
// A notification is skipped while the user is looking at the page it would
// open (Presence), so an open chat does not buzz the phone for every
// message.
package push

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/dseif0x/agents-operator/internal/store"
)

const (
	keyPublic  = "vapid_public_key"
	keyPrivate = "vapid_private_key"
)

// Notification is what the service worker shows.
type Notification struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	// URL is the app path opened on a tap (and compared with Presence).
	URL string `json:"url"`
	// Tag groups notifications: a newer one with the same tag replaces it.
	Tag string `json:"tag,omitempty"`
}

// Service sends notifications.
type Service struct {
	Store store.Store
	Log   *slog.Logger
	// Subject identifies the sender to push services (a URL or mailto:).
	Subject string
	// send is webpush.SendNotification, swapped out in tests.
	send func(msg []byte, s *webpush.Subscription, o *webpush.Options) (*http.Response, error)

	mu       sync.Mutex
	pub      string
	priv     string
	presence map[string]presence
}

type presence struct {
	path  string
	until time.Time
}

// PresenceTTL is how long a presence report counts; the UI repeats it
// while visible.
const PresenceTTL = 70 * time.Second

func (s *Service) keys(ctx context.Context) (string, string, error) {
	s.mu.Lock()
	pub, priv := s.pub, s.priv
	s.mu.Unlock()
	if pub != "" {
		return pub, priv, nil
	}
	pub, err := s.Store.Settings().Get(ctx, keyPublic)
	if err != nil {
		return "", "", err
	}
	priv, err = s.Store.Settings().Get(ctx, keyPrivate)
	if err != nil {
		return "", "", err
	}
	if pub == "" || priv == "" {
		newPriv, newPub, err := webpush.GenerateVAPIDKeys()
		if err != nil {
			return "", "", err
		}
		// Private first: whoever wins the race for it wins the pair.
		if priv, err = s.Store.Settings().SetIfAbsent(ctx, keyPrivate, newPriv); err != nil {
			return "", "", err
		}
		if priv == newPriv {
			_, _ = s.Store.Settings().SetIfAbsent(ctx, keyPublic, newPub)
		}
		if pub, err = s.Store.Settings().Get(ctx, keyPublic); err != nil || pub == "" {
			return "", "", errors.New("vapid keys not settled yet")
		}
	}
	s.mu.Lock()
	s.pub, s.priv = pub, priv
	s.mu.Unlock()
	return pub, priv, nil
}

// PublicKey is the VAPID public key browsers subscribe with.
func (s *Service) PublicKey(ctx context.Context) (string, error) {
	pub, _, err := s.keys(ctx)
	return pub, err
}

// Present records that userID is looking at path (visible), or no longer
// (path "").
func (s *Service) Present(userID, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.presence == nil {
		s.presence = map[string]presence{}
	}
	if path == "" {
		delete(s.presence, userID)
		return
	}
	s.presence[userID] = presence{path: path, until: time.Now().Add(PresenceTTL)}
}

// watching reports whether userID is looking at path right now.
func (s *Service) watching(userID, path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.presence[userID]
	return ok && p.path == path && time.Now().Before(p.until)
}

// Notify sends n to every subscription of userID, unless the user is
// looking at n.URL. Errors are logged; it never blocks the caller.
func (s *Service) Notify(userID string, n Notification) {
	if s == nil || s.watching(userID, n.URL) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := s.Send(ctx, userID, n); err != nil {
			s.Log.Warn("push notification failed", "err", err)
		}
	}()
}

// Send delivers n to every subscription of userID now and returns how many
// push services accepted it. Subscriptions the push service no longer knows
// are deleted.
func (s *Service) Send(ctx context.Context, userID string, n Notification) (int, error) {
	subs, err := s.Store.Push().List(ctx, userID)
	if err != nil || len(subs) == 0 {
		return 0, err
	}
	pub, priv, err := s.keys(ctx)
	if err != nil {
		return 0, err
	}
	payload, err := json.Marshal(n)
	if err != nil {
		return 0, err
	}
	send := s.send
	if send == nil {
		send = func(msg []byte, sub *webpush.Subscription, o *webpush.Options) (*http.Response, error) {
			return webpush.SendNotificationWithContext(ctx, msg, sub, o)
		}
	}
	subject := s.Subject
	if subject == "" {
		subject = "mailto:agents-operator@localhost"
	}
	sent := 0
	var errs []error
	for _, sub := range subs {
		resp, err := send(payload, &webpush.Subscription{Endpoint: sub.Endpoint, Keys: webpush.Keys{P256dh: sub.P256dh, Auth: sub.Auth}}, &webpush.Options{
			Subscriber: subject, VAPIDPublicKey: pub, VAPIDPrivateKey: priv, TTL: 3600, Urgency: webpush.UrgencyHigh, Topic: topic(n.Tag),
		})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		_ = resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
			_ = s.Store.Push().Delete(ctx, "", sub.Endpoint)
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			sent++
		default:
			errs = append(errs, errors.New("push service answered "+resp.Status))
		}
	}
	return sent, errors.Join(errs...)
}

// topic turns a tag into a Web Push topic (at most 32 URL-safe characters),
// so a newer notification replaces an undelivered older one.
func topic(tag string) string {
	out := make([]byte, 0, 32)
	for i := 0; i < len(tag) && len(out) < 32; i++ {
		c := tag[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			out = append(out, c)
		}
	}
	return string(out)
}
