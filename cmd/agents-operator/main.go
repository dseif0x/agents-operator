// Command agents-operator is the hub: REST API, WebSocket terminal proxy,
// Kubernetes reconciler and the embedded web UI, in one binary.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/dseif0x/agents-operator/internal/api"
	"github.com/dseif0x/agents-operator/internal/auth"
	"github.com/dseif0x/agents-operator/internal/config"
	"github.com/dseif0x/agents-operator/internal/github"
	"github.com/dseif0x/agents-operator/internal/k8s"
	"github.com/dseif0x/agents-operator/internal/reconcile"
	"github.com/dseif0x/agents-operator/internal/session"
	"github.com/dseif0x/agents-operator/internal/store"
	"github.com/dseif0x/agents-operator/internal/term"
	"github.com/dseif0x/agents-operator/internal/ui"
)

// version is set by the linker from the git tag.
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "hash-password":
			os.Exit(hashPassword(os.Args[2:]))
		case "version":
			fmt.Println(version)
			return
		}
	}
	if err := run(); err != nil {
		slog.Error("agents-operator exited", "err", err)
		os.Exit(1)
	}
}

// hashPassword prints an argon2id hash for auth.adminPasswordHash. The
// password is read from the AGENTS_OPERATOR_PASSWORD env var or stdin.
func hashPassword(_ []string) int {
	pw := os.Getenv("AGENTS_OPERATOR_PASSWORD")
	if pw == "" {
		var line string
		if _, err := fmt.Fscanln(os.Stdin, &line); err != nil {
			fmt.Fprintln(os.Stderr, "usage: AGENTS_OPERATOR_PASSWORD=... agents-operator hash-password  (or pipe the password on stdin)")
			return 2
		}
		pw = line
	}
	h, err := auth.HashPassword(pw)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(h)
	return 0
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)
	log.Info("agents-operator starting", "version", version, "namespace", cfg.Namespace, "public_url", cfg.PublicURL.String(), "ui", ui.Built)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Storage.
	st, err := store.Open(ctx, cfg.DatabaseURL, log)
	if err != nil {
		return err
	}
	defer st.Close()
	adminHash := cfg.AdminPasswordHash
	if adminHash == "" && cfg.AdminPassword != "" {
		if adminHash, err = auth.HashPassword(cfg.AdminPassword); err != nil {
			return err
		}
	}
	if adminHash != "" {
		if _, err := st.Users().UpsertPassword(ctx, cfg.AdminUsername, adminHash); err != nil {
			return fmt.Errorf("bootstrap admin user: %w", err)
		}
		log.Info("admin user ready", "username", cfg.AdminUsername)
	}

	// Kubernetes.
	cs, err := k8s.NewClientset(cfg.Kubeconfig)
	if err != nil {
		return err
	}
	inf := k8s.NewInformers(cs, cfg.Namespace, 10*time.Minute)
	if err := inf.Start(ctx); err != nil {
		return err
	}
	log.Info("informers synced")

	// Metrics.
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	session.RegisterMetrics(reg)

	// Wiring.
	rcfg := reconcile.Config{
		Namespace: cfg.Namespace, RunnerImage: cfg.RunnerImage, RunnerImageTag: cfg.RunnerImageTag, ImagePullPolicy: cfg.RunnerImagePullPolicy,
		DefaultStorageClass: cfg.DefaultStorageClass, DefaultPVCSize: cfg.DefaultPVCSize, DefaultResources: cfg.DefaultResources,
		NodeSelector: cfg.RunnerNodeSelector, Tolerations: cfg.RunnerTolerations, RuntimeClass: cfg.RunnerRuntimeClass, ExtraEnv: cfg.RunnerExtraEnv,
	}
	proxy := &term.Proxy{Resolver: &reconcile.Resolver{Informers: inf, Namespace: cfg.Namespace}, OriginPatterns: cfg.AllowedHosts, Log: log}
	creds := &session.Credentials{Store: st, CS: cs, Namespace: cfg.Namespace}
	svc := &session.Service{
		Store: st, Term: proxy, Broker: session.NewBroker(), Creds: creds, GitHub: &github.Client{}, IdleStopAfter: cfg.IdleStopAfter, Log: log,
		Defaults: session.Defaults{PVCSize: cfg.DefaultPVCSize, StorageClass: cfg.DefaultStorageClass, Autonomous: true},
	}
	rec := reconcile.New(rcfg, st, cs, inf, svc, cfg.ReconcileInterval, log)
	svc.Orch = rec

	srv := &api.Server{
		Cfg: cfg, Store: st, Sessions: svc, Creds: creds,
		Auth:    auth.PasswordAuthenticator{Users: st.Users()},
		Cookies: auth.NewSessions(cfg.CookieSecret, cfg.Secure(), st.Users()),
		Limiter: auth.NewRateLimiter(10, 15*time.Minute),
		Term:    proxy,
		Ready: func() bool {
			pctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			return inf.Synced() && st.Ping(pctx) == nil
		},
		UI:      ui.Handler(),
		Metrics: promhttp.HandlerFor(reg, promhttp.HandlerOpts{}),
		Log:     log,
	}

	go rec.Run(ctx)
	go svc.RunPoller(ctx, cfg.StatusPollEvery)

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.ListenAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return httpSrv.Shutdown(sctx)
}
