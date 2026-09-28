package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dseif0x/agents-operator/internal/store"
)

func TestPassword(t *testing.T) {
	h, err := HashPassword("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(h, "hunter2") {
		t.Fatal("valid password rejected")
	}
	if VerifyPassword(h, "hunter3") || VerifyPassword("garbage", "hunter2") {
		t.Fatal("invalid password accepted")
	}
}

func TestLogin(t *testing.T) {
	st := store.NewMemory()
	h, _ := HashPassword("pw")
	_ = st.Users().Create(context.Background(), &store.User{Username: "u", PasswordHash: h})
	a := PasswordAuthenticator{Users: st.Users()}
	if _, err := a.Login(context.Background(), "u", "pw"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Login(context.Background(), "u", "no"); err != ErrBadCredentials {
		t.Fatalf("err = %v", err)
	}
	if _, err := a.Login(context.Background(), "nobody", "pw"); err != ErrBadCredentials {
		t.Fatalf("err = %v", err)
	}
}

func TestCookieSessions(t *testing.T) {
	st := store.NewMemory()
	u := &store.User{Username: "u", PasswordHash: "x"}
	_ = st.Users().Create(context.Background(), u)
	s := NewSessions([]byte("0123456789abcdef0123456789abcdef"), true, st.Users())

	rec := httptest.NewRecorder()
	p := s.Issue(rec, u)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie flags: %+v", cookies)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookies[0])
	got, err := s.Read(req)
	if err != nil || got.User.ID != u.ID || got.Nonce != p.Nonce {
		t.Fatalf("Read = %+v, %v", got, err)
	}
	// CSRF token round trip.
	req.Header.Set(CSRFHeader, s.CSRFToken(p))
	if !s.CheckCSRF(req, got) {
		t.Fatal("csrf should pass")
	}
	req.Header.Set(CSRFHeader, "nope")
	if s.CheckCSRF(req, got) {
		t.Fatal("csrf should fail")
	}
	// Tampering breaks the signature.
	bad := *cookies[0]
	bad.Value = "x" + bad.Value
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.AddCookie(&bad)
	if _, err := s.Read(req2); err == nil {
		t.Fatal("tampered cookie accepted")
	}
	// A second login rotates the nonce.
	rec2 := httptest.NewRecorder()
	p2 := s.Issue(rec2, u)
	if p2.Nonce == p.Nonce {
		t.Fatal("nonce not rotated")
	}
	// Different secret rejects.
	other := NewSessions([]byte("ffffffffffffffffffffffffffffffff"), true, st.Users())
	if _, err := other.Read(req); err == nil {
		t.Fatal("foreign secret accepted")
	}
}

func TestRateLimiter(t *testing.T) {
	now := time.Now()
	l := NewRateLimiter(3, 15*time.Minute)
	l.now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		if !l.Allowed("1.2.3.4") {
			t.Fatal("should be allowed")
		}
		l.Fail("1.2.3.4")
	}
	l.Fail("1.2.3.4")
	if l.Allowed("1.2.3.4") {
		t.Fatal("should be locked out")
	}
	if !l.Allowed("5.6.7.8") {
		t.Fatal("other ip affected")
	}
	now = now.Add(16 * time.Minute)
	if !l.Allowed("1.2.3.4") {
		t.Fatal("lockout should expire")
	}
	l.Fail("1.2.3.4")
	l.Reset("1.2.3.4")
	l.Fail("1.2.3.4")
	l.Fail("1.2.3.4")
	if !l.Allowed("1.2.3.4") {
		t.Fatal("reset should clear count")
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	if ClientIP(r) != "10.0.0.1" {
		t.Fatal(ClientIP(r))
	}
	r.Header.Set("X-Forwarded-For", "1.1.1.1, 2.2.2.2")
	if ClientIP(r) != "2.2.2.2" {
		t.Fatal(ClientIP(r))
	}
}
