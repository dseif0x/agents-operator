package config

import (
	"strings"
	"testing"
)

func env(m map[string]string) lookup {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestLoadRequired(t *testing.T) {
	_, err := load(env(map[string]string{}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"DATABASE_URL", "PUBLIC_URL", "COOKIE_SECRET"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %s: %v", want, err)
		}
	}
}

func TestLoadDefaults(t *testing.T) {
	c, err := load(env(map[string]string{
		"AGENTHUB_DATABASE_URL":       "postgres://x",
		"AGENTHUB_PUBLIC_URL":         "https://agenthub.example.com",
		"AGENTHUB_COOKIE_SECRET":      strings.Repeat("ab", 32),
		"AGENTHUB_NAMESPACE":          "agenthub",
		"AGENTHUB_ALLOWED_HOSTS":      "localhost:8080, Other.Example.com",
		"AGENTHUB_RUNNER_TOLERATIONS": `[{"key":"arm","operator":"Exists"}]`,
		"AGENTHUB_IDLE_STOP_AFTER":    "4h",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":8080" || c.DefaultPVCSize != "20Gi" || c.RunnerImageTag != "latest" {
		t.Errorf("defaults wrong: %+v", c)
	}
	if !c.Secure() || c.PublicHost() != "agenthub.example.com" {
		t.Errorf("public url wrong: %+v", c.PublicURL)
	}
	want := []string{"localhost:8080", "other.example.com", "agenthub.example.com"}
	if strings.Join(c.AllowedHosts, ",") != strings.Join(want, ",") {
		t.Errorf("allowed hosts = %v", c.AllowedHosts)
	}
	if len(c.RunnerTolerations) != 1 || c.RunnerTolerations[0].Key != "arm" {
		t.Errorf("tolerations = %+v", c.RunnerTolerations)
	}
	if c.IdleStopAfter.Hours() != 4 {
		t.Errorf("idle = %v", c.IdleStopAfter)
	}
	if c.DefaultResources.Limits.CPU != "2" {
		t.Errorf("resources = %+v", c.DefaultResources)
	}
}

func TestLoadBadValues(t *testing.T) {
	_, err := load(env(map[string]string{
		"AGENTHUB_DATABASE_URL":  "postgres://x",
		"AGENTHUB_PUBLIC_URL":    "not a url",
		"AGENTHUB_COOKIE_SECRET": "abcd",
		"AGENTHUB_LISTEN_ADDR":   "nope",
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"PUBLIC_URL", "COOKIE_SECRET", "LISTEN_ADDR"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %s: %v", want, err)
		}
	}
}
