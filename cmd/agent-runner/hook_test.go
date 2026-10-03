package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHookForward(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = r.Method + " " + r.URL.Path + " " + r.Header.Get("Content-Type") + " " + string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	hookForward(srv.URL+"/hook", strings.NewReader(`{"hook_event_name":"Stop"}`))
	if got != `POST /hook application/json {"hook_event_name":"Stop"}` {
		t.Fatalf("forwarded %q", got)
	}
	// Nothing to send, or nobody listening: silent either way.
	hookForward(srv.URL+"/hook", strings.NewReader(""))
	hookForward("http://127.0.0.1:1/hook", strings.NewReader(`{"hook_event_name":"Stop"}`))
	t.Setenv("RUNNER_HOOK_URL", "")
	if hookURL() != "http://127.0.0.1:7682/hook" {
		t.Fatalf("default url = %s", hookURL())
	}
	t.Setenv("RUNNER_HOOK_URL", "http://x/y")
	if hookURL() != "http://x/y" {
		t.Fatalf("env url = %s", hookURL())
	}
}
