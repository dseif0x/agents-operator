package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dseif0x/agents-operator/internal/store"
)

// Principal is the authenticated user attached to a request.
type Principal struct {
	User *store.User
	// Nonce is unique per login and is what the CSRF token is derived from.
	Nonce string
}

// Authenticator turns credentials into a user. PasswordAuthenticator is the
// v1 implementation; an OIDC one would exchange a code instead.
type Authenticator interface {
	Login(ctx context.Context, username, password string) (*store.User, error)
}

// PasswordAuthenticator checks username/password against the users table.
type PasswordAuthenticator struct {
	Users store.Users
}

// Login implements Authenticator.
func (a PasswordAuthenticator) Login(ctx context.Context, username, password string) (*store.User, error) {
	u, err := a.Users.GetByUsername(ctx, strings.TrimSpace(username))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Burn the same time as a real check to keep timing uniform.
			VerifyPassword("$argon2id$v=19$m=65536,t=3,p=4$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", password)
			return nil, ErrBadCredentials
		}
		return nil, err
	}
	if u.Disabled || !VerifyPassword(u.PasswordHash, password) {
		return nil, ErrBadCredentials
	}
	return u, nil
}

// CookieName is the session cookie.
const CookieName = "agents_operator_session"

// SessionTTL is how long a login lasts.
const SessionTTL = 30 * 24 * time.Hour

// Sessions issues and verifies HMAC-signed cookie sessions. Being stateless
// they survive hub restarts; logout is done by clearing the cookie.
type Sessions struct {
	secret []byte
	secure bool
	users  store.Users
}

// NewSessions returns a cookie session manager.
func NewSessions(secret []byte, secure bool, users store.Users) *Sessions {
	return &Sessions{secret: secret, secure: secure, users: users}
}

func (s *Sessions) sign(payload string) string {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Issue sets a fresh session cookie for the user and returns the principal.
// Every call generates a new nonce, so the cookie rotates on each login.
func (s *Sessions) Issue(w http.ResponseWriter, u *store.User) *Principal {
	nonce := RandomHex(16)
	exp := time.Now().Add(SessionTTL).Unix()
	payload := u.ID + "|" + strconv.FormatInt(exp, 10) + "|" + nonce
	value := base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + s.sign(payload)
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: value, Path: "/", HttpOnly: true, Secure: s.secure,
		SameSite: http.SameSiteLaxMode, Expires: time.Unix(exp, 0),
	})
	return &Principal{User: u, Nonce: nonce}
}

// Clear removes the session cookie.
func (s *Sessions) Clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: "", Path: "/", HttpOnly: true, Secure: s.secure,
		SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

// Read verifies the cookie on r and loads the user.
func (s *Sessions) Read(r *http.Request) (*Principal, error) {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return nil, errors.New("no session")
	}
	enc, sig, ok := strings.Cut(c.Value, ".")
	if !ok {
		return nil, errors.New("malformed session")
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return nil, errors.New("malformed session")
	}
	payload := string(raw)
	if subtle.ConstantTimeCompare([]byte(s.sign(payload)), []byte(sig)) != 1 {
		return nil, errors.New("bad signature")
	}
	parts := strings.Split(payload, "|")
	if len(parts) != 3 {
		return nil, errors.New("malformed session")
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return nil, errors.New("session expired")
	}
	u, err := s.users.GetByID(r.Context(), parts[0])
	if err != nil {
		return nil, errors.New("unknown user")
	}
	if u.Disabled {
		return nil, errors.New("user disabled")
	}
	return &Principal{User: u, Nonce: parts[2]}, nil
}

// CSRFHeader is the header state-changing requests must carry.
const CSRFHeader = "X-CSRF-Token"

// CSRFToken derives the CSRF token for a principal. It is bound to the
// login nonce, so it cannot be reused across logins.
func (s *Sessions) CSRFToken(p *Principal) string {
	return s.sign("csrf|" + p.User.ID + "|" + p.Nonce)
}

// CheckCSRF verifies the CSRF header on r.
func (s *Sessions) CheckCSRF(r *http.Request, p *Principal) bool {
	got := r.Header.Get(CSRFHeader)
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(s.CSRFToken(p))) == 1
}

// RateLimiter locks out a source IP after too many failed logins.
type RateLimiter struct {
	mu       sync.Mutex
	max      int
	lockout  time.Duration
	failures map[string]*failure
	now      func() time.Time
}

type failure struct {
	count int
	first time.Time
	until time.Time
}

// NewRateLimiter allows max failures per IP within lockout, then blocks the
// IP for lockout.
func NewRateLimiter(max int, lockout time.Duration) *RateLimiter {
	return &RateLimiter{max: max, lockout: lockout, failures: map[string]*failure{}, now: time.Now}
}

// Allowed reports whether ip may attempt a login now.
func (l *RateLimiter) Allowed(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, ok := l.failures[ip]
	if !ok {
		return true
	}
	now := l.now()
	if !f.until.IsZero() {
		if now.Before(f.until) {
			return false
		}
		delete(l.failures, ip)
		return true
	}
	if now.Sub(f.first) > l.lockout {
		delete(l.failures, ip)
	}
	return true
}

// Fail records a failed attempt from ip.
func (l *RateLimiter) Fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	f, ok := l.failures[ip]
	if !ok || now.Sub(f.first) > l.lockout {
		f = &failure{first: now}
		l.failures[ip] = f
	}
	f.count++
	if f.count >= l.max {
		f.until = now.Add(l.lockout)
	}
}

// Reset clears failures for ip after a successful login.
func (l *RateLimiter) Reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, ip)
}

// ClientIP extracts the source IP, honouring X-Forwarded-For from the
// ingress. Only the last hop is trusted, which is what Traefik sets.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
			return ip
		}
	}
	if xri := r.Header.Get("X-Real-Ip"); xri != "" {
		return xri
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
