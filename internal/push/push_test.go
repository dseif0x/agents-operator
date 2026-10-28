package push

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/dseif0x/agents-operator/internal/store"
)

func TestSend(t *testing.T) {
	st := store.NewMemory()
	ctx := context.Background()
	var mu sync.Mutex
	var got []string
	s := &Service{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Subject: "https://hub.example"}
	s.send = func(msg []byte, sub *webpush.Subscription, o *webpush.Options) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		var n Notification
		_ = json.Unmarshal(msg, &n)
		got = append(got, sub.Endpoint+" "+n.Title+" "+o.Topic)
		if o.VAPIDPublicKey == "" || o.VAPIDPrivateKey == "" || o.Subscriber != "https://hub.example" {
			t.Errorf("options = %+v", o)
		}
		status := http.StatusCreated
		if strings.HasSuffix(sub.Endpoint, "/gone") {
			status = http.StatusGone
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(""))}, nil
	}

	// The key pair is created once and kept.
	k1, err := s.PublicKey(ctx)
	if err != nil || k1 == "" {
		t.Fatalf("key = %q, %v", k1, err)
	}
	other := &Service{Store: st}
	if k2, _ := other.PublicKey(ctx); k2 != k1 {
		t.Fatal("a second hub got another key")
	}

	for _, ep := range []string{"https://push.example/phone", "https://push.example/gone"} {
		_ = st.Push().Save(ctx, &store.PushSubscription{Endpoint: ep, UserID: "u1", P256dh: "p", Auth: "a"})
	}
	_ = st.Push().Save(ctx, &store.PushSubscription{Endpoint: "https://push.example/other-user", UserID: "u2", P256dh: "p", Auth: "a"})
	n, err := s.Send(ctx, "u1", Notification{Title: "launch · reviewer", Body: "LGTM", URL: "/chats/c1", Tag: "chat-c1/x"})
	if err != nil || n != 1 {
		t.Fatalf("sent %d, %v", n, err)
	}
	if len(got) != 2 || !strings.Contains(got[0], "launch · reviewer chat-c1x") {
		t.Fatalf("sent to %v", got)
	}
	// The gone subscription was dropped.
	if subs, _ := st.Push().List(ctx, "u1"); len(subs) != 1 || subs[0].Endpoint != "https://push.example/phone" {
		t.Fatalf("subscriptions = %+v", subs)
	}

	// Looking at the page: no notification.
	got = nil
	s.Present("u1", "/chats/c1")
	s.Notify("u1", Notification{Title: "x", URL: "/chats/c1"})
	s.Notify("u1", Notification{Title: "y", URL: "/chats/c2"})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || !strings.Contains(got[0], " y ") {
		t.Fatalf("with presence sent %v", got)
	}
	s.Present("u1", "")
	if s.watching("u1", "/chats/c1") {
		t.Fatal("presence not cleared")
	}
}

func TestTopic(t *testing.T) {
	if got := topic("session-1234/../ü" + strings.Repeat("x", 40)); len(got) != 32 || strings.ContainsAny(got, "/.ü") {
		t.Fatalf("topic = %q", got)
	}
}

// TestRealEncryption runs the real webpush encryption and VAPID signing
// against a local push endpoint with a browser-style subscription.
func TestRealEncryption(t *testing.T) {
	var gotAuth, gotEnc string
	var gotLen int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotAuth, gotEnc, gotLen = r.Header.Get("Authorization"), r.Header.Get("Content-Encoding"), len(b)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	st := store.NewMemory()
	ctx := context.Background()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	_ = st.Push().Save(ctx, &store.PushSubscription{Endpoint: srv.URL + "/sub", UserID: "u1",
		P256dh: base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()), Auth: base64.RawURLEncoding.EncodeToString(auth)})
	s := &Service{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Subject: "https://hub.example"}
	client := srv.Client()
	s.send = func(msg []byte, sub *webpush.Subscription, o *webpush.Options) (*http.Response, error) {
		o.HTTPClient = client
		return webpush.SendNotification(msg, sub, o)
	}
	n, err := s.Send(ctx, "u1", Notification{Title: "t", Body: "b", URL: "/"})
	if err != nil || n != 1 {
		t.Fatalf("sent %d, %v", n, err)
	}
	if !strings.HasPrefix(gotAuth, "vapid t=") || gotEnc != "aes128gcm" || gotLen == 0 {
		t.Fatalf("auth=%q enc=%q len=%d", gotAuth, gotEnc, gotLen)
	}
}
