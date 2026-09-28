// Package config loads the hub configuration from AGENTS_OPERATOR_* environment
// variables and fails fast on anything missing or malformed.
package config

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Prefix is prepended to every environment variable name.
const Prefix = "AGENTS_OPERATOR_"

// Resources is a CPU/memory request and limit pair in Kubernetes quantity
// syntax. Kept free of k8s types so it can live in the store too.
type Resources struct {
	Requests ResourceList `json:"requests"`
	Limits   ResourceList `json:"limits"`
}

// ResourceList is one side of Resources.
type ResourceList struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// Toleration mirrors corev1.Toleration without importing it.
type Toleration struct {
	Key               string `json:"key,omitempty"`
	Operator          string `json:"operator,omitempty"`
	Value             string `json:"value,omitempty"`
	Effect            string `json:"effect,omitempty"`
	TolerationSeconds *int64 `json:"tolerationSeconds,omitempty"`
}

// Config is the fully parsed hub configuration.
type Config struct {
	ListenAddr  string
	PublicURL   *url.URL
	DatabaseURL string
	Namespace   string
	// Kubeconfig is only used outside the cluster (make dev).
	Kubeconfig string

	RunnerImage           string
	RunnerImageTag        string
	RunnerImagePullPolicy string
	DefaultStorageClass   string
	DefaultPVCSize        string
	DefaultResources      Resources
	RunnerNodeSelector    map[string]string
	RunnerTolerations     []Toleration
	RunnerRuntimeClass    string
	// RunnerExtraEnv is injected into every session pod (non-secret).
	RunnerExtraEnv map[string]string

	CookieSecret      []byte
	AdminUsername     string
	AdminPasswordHash string
	// AdminPassword is a plaintext alternative to AdminPasswordHash, hashed
	// at startup. The chart uses it for the generated password.
	AdminPassword string
	IdleStopAfter time.Duration
	AllowedHosts  []string
	LogLevel      string

	ReconcileInterval time.Duration
	StatusPollEvery   time.Duration
	// HubPodLabels lets the reconciler find itself for NetworkPolicy docs;
	// unused by code today, kept for the chart's sake.
}

// RunnerImageRef returns image:tag.
func (c *Config) RunnerImageRef() string {
	return c.RunnerImage + ":" + c.RunnerImageTag
}

// PublicHost is the host[:port] part of PublicURL.
func (c *Config) PublicHost() string { return c.PublicURL.Host }

// Secure reports whether cookies must carry the Secure flag.
func (c *Config) Secure() bool { return c.PublicURL.Scheme == "https" }

type lookup func(string) (string, bool)

// Load reads the configuration from the environment.
func Load() (*Config, error) {
	return load(os.LookupEnv)
}

func load(get lookup) (*Config, error) {
	var errs []error
	str := func(key, def string) string {
		if v, ok := get(Prefix + key); ok && v != "" {
			return v
		}
		return def
	}
	required := func(key string) string {
		v := str(key, "")
		if v == "" {
			errs = append(errs, fmt.Errorf("%s%s is required", Prefix, key))
		}
		return v
	}
	dur := func(key string, def time.Duration) time.Duration {
		v := str(key, "")
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s%s: %w", Prefix, key, err))
		}
		return d
	}
	jsonInto := func(key string, dst any) {
		v := str(key, "")
		if v == "" {
			return
		}
		if err := json.Unmarshal([]byte(v), dst); err != nil {
			errs = append(errs, fmt.Errorf("%s%s: invalid JSON: %w", Prefix, key, err))
		}
	}

	c := &Config{
		ListenAddr:            str("LISTEN_ADDR", ":8080"),
		DatabaseURL:           required("DATABASE_URL"),
		Kubeconfig:            str("KUBECONFIG", ""),
		RunnerImage:           str("RUNNER_IMAGE", "ghcr.io/dseif0x/agents-operator-runner"),
		RunnerImageTag:        str("RUNNER_IMAGE_TAG", "latest"),
		RunnerImagePullPolicy: str("RUNNER_IMAGE_PULL_POLICY", "IfNotPresent"),
		DefaultStorageClass:   str("DEFAULT_STORAGE_CLASS", ""),
		DefaultPVCSize:        str("DEFAULT_PVC_SIZE", "20Gi"),
		RunnerRuntimeClass:    str("RUNNER_RUNTIME_CLASS", ""),
		AdminUsername:         str("ADMIN_USERNAME", "admin"),
		AdminPasswordHash:     str("ADMIN_PASSWORD_HASH", ""),
		AdminPassword:         str("ADMIN_PASSWORD", ""),
		IdleStopAfter:         dur("IDLE_STOP_AFTER", 0),
		LogLevel:              str("LOG_LEVEL", "info"),
		ReconcileInterval:     dur("RECONCILE_INTERVAL", 30*time.Second),
		StatusPollEvery:       dur("STATUS_POLL_INTERVAL", 10*time.Second),
		DefaultResources: Resources{
			Requests: ResourceList{CPU: str("DEFAULT_CPU_REQUEST", "250m"), Memory: str("DEFAULT_MEMORY_REQUEST", "512Mi")},
			Limits:   ResourceList{CPU: str("DEFAULT_CPU_LIMIT", "2"), Memory: str("DEFAULT_MEMORY_LIMIT", "4Gi")},
		},
	}
	jsonInto("DEFAULT_RESOURCES", &c.DefaultResources)
	jsonInto("RUNNER_NODE_SELECTOR", &c.RunnerNodeSelector)
	jsonInto("RUNNER_TOLERATIONS", &c.RunnerTolerations)
	jsonInto("RUNNER_EXTRA_ENV", &c.RunnerExtraEnv)

	if raw := required("PUBLIC_URL"); raw != "" {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			errs = append(errs, fmt.Errorf("%sPUBLIC_URL must be an absolute http(s) URL", Prefix))
		} else {
			c.PublicURL = u
		}
	}

	if raw := required("COOKIE_SECRET"); raw != "" {
		b, err := hex.DecodeString(raw)
		if err != nil || len(b) < 32 {
			errs = append(errs, fmt.Errorf("%sCOOKIE_SECRET must be at least 32 bytes hex", Prefix))
		}
		c.CookieSecret = b
	}

	c.Namespace = str("NAMESPACE", "")
	if c.Namespace == "" {
		if b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
			c.Namespace = strings.TrimSpace(string(b))
		}
	}
	if c.Namespace == "" {
		c.Namespace = "default"
	}

	if v := str("ALLOWED_HOSTS", ""); v != "" {
		for _, h := range strings.Split(v, ",") {
			if h = strings.TrimSpace(h); h != "" {
				c.AllowedHosts = append(c.AllowedHosts, strings.ToLower(h))
			}
		}
	}
	if c.PublicURL != nil {
		c.AllowedHosts = appendUnique(c.AllowedHosts, strings.ToLower(c.PublicURL.Host))
		if host := c.PublicURL.Hostname(); host != c.PublicURL.Host {
			c.AllowedHosts = appendUnique(c.AllowedHosts, strings.ToLower(host))
		}
	}
	if _, _, err := splitAddr(c.ListenAddr); err != nil {
		errs = append(errs, fmt.Errorf("%sLISTEN_ADDR: %w", Prefix, err))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

func splitAddr(addr string) (string, int, error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return "", 0, errors.New("missing port")
	}
	port, err := strconv.Atoi(addr[i+1:])
	if err != nil {
		return "", 0, fmt.Errorf("bad port: %w", err)
	}
	return addr[:i], port, nil
}
