package config

import (
	"encoding/json"
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
		"AGENTS_OPERATOR_DATABASE_URL":       "postgres://x",
		"AGENTS_OPERATOR_PUBLIC_URL":         "https://agents-operator.example.com",
		"AGENTS_OPERATOR_COOKIE_SECRET":      strings.Repeat("ab", 32),
		"AGENTS_OPERATOR_NAMESPACE":          "agents-operator",
		"AGENTS_OPERATOR_ALLOWED_HOSTS":      "localhost:8080, Other.Example.com",
		"AGENTS_OPERATOR_RUNNER_TOLERATIONS": `[{"key":"arm","operator":"Exists"}]`,
		"AGENTS_OPERATOR_IDLE_STOP_AFTER":    "4h",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":8080" || c.DefaultPVCSize != "20Gi" || c.RunnerImageTag != "latest" {
		t.Errorf("defaults wrong: %+v", c)
	}
	if !c.Secure() || c.PublicHost() != "agents-operator.example.com" {
		t.Errorf("public url wrong: %+v", c.PublicURL)
	}
	want := []string{"localhost:8080", "other.example.com", "agents-operator.example.com"}
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
		"AGENTS_OPERATOR_DATABASE_URL":  "postgres://x",
		"AGENTS_OPERATOR_PUBLIC_URL":    "not a url",
		"AGENTS_OPERATOR_COOKIE_SECRET": "abcd",
		"AGENTS_OPERATOR_LISTEN_ADDR":   "nope",
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

func TestResourceListJSON(t *testing.T) {
	var r Resources
	// A chart value with a bare number and an extended resource.
	if err := json.Unmarshal([]byte(`{"requests":{"cpu":"250m"},"limits":{"cpu":2,"memory":"4Gi","nvidia.com/gpu":"1"}}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.Limits.CPU != "2" || r.Limits.Memory != "4Gi" || r.Limits.Extended["nvidia.com/gpu"] != "1" || r.Requests.CPU != "250m" || r.Requests.Extended != nil {
		t.Fatalf("parsed = %+v", r)
	}
	out, _ := json.Marshal(r.Limits)
	if string(out) != `{"cpu":"2","memory":"4Gi","nvidia.com/gpu":"1"}` {
		t.Fatalf("marshal = %s", out)
	}
	// Empty sides marshal as {} and round-trip.
	out, _ = json.Marshal(ResourceList{})
	if string(out) != `{}` {
		t.Fatalf("empty = %s", out)
	}
	if err := json.Unmarshal([]byte(`{"cpu":true}`), &r.Limits); err == nil {
		t.Fatal("non-quantity accepted")
	}
}
