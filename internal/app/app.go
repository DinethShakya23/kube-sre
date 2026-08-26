// Package app wires the server together: database, recorder, audit, tools, the
// agent and the HTTP API. It owns startup and shutdown order.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/DinethShakya23/kube-sre/internal/agent"
	"github.com/DinethShakya23/kube-sre/internal/api"
	"github.com/DinethShakya23/kube-sre/internal/audit"
	"github.com/DinethShakya23/kube-sre/internal/cluster"
	"github.com/DinethShakya23/kube-sre/internal/config"
	"github.com/DinethShakya23/kube-sre/internal/events"
	"github.com/DinethShakya23/kube-sre/internal/helm"
	"github.com/DinethShakya23/kube-sre/internal/kube"
	"github.com/DinethShakya23/kube-sre/internal/llm"
	"github.com/DinethShakya23/kube-sre/internal/loki"
	"github.com/DinethShakya23/kube-sre/internal/nsguard"
	"github.com/DinethShakya23/kube-sre/internal/playbooks"
	"github.com/DinethShakya23/kube-sre/internal/prom"
	"github.com/DinethShakya23/kube-sre/internal/recorder"
	"github.com/DinethShakya23/kube-sre/internal/schema"
	"github.com/DinethShakya23/kube-sre/internal/store"
)

// Version is the software version.
const Version = "0.1.0"

// App is a running server's parts.
type App struct {
	Cfg      *config.Config
	DB       *store.DB
	Recorder *recorder.Recorder
	Audit    *audit.Log
	Emitter  *events.Emitter
	Agent    *agent.Agent
	Server   *api.Server
}

// Check validates configuration and logs what would make the server misbehave.
// Misconfiguration is never fatal on its own here, apart from the two cases that
// change what the server is willing to do: REQUIRE_AUTH with no keys, and a
// setting that cannot be read.
func Check(cfg *config.Config) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("configuration error: %w", err)
	}
	// A guard entry that cannot match protects nothing, and every parser discards
	// silently. Logged loudly at startup and never fatal: an operator's typo must not
	// take the agent offline, only become impossible to miss.
	for _, w := range cfg.Warnings() {
		slog.Warn("guard configuration problem: " + w)
	}
	for _, w := range cfg.LLMWarnings() {
		slog.Warn(w)
	}
	// Fail fast, before the port opens. An operator who set REQUIRE_AUTH asked for
	// exactly one thing: that this server never serve an unauthenticated request.
	if cfg.RequireAuth && cfg.OpenAccess() {
		return errors.New("REQUIRE_AUTH=true but no API keys are configured. Without keys every unauthenticated caller is " +
			"treated as admin, with full approval gated write access to the cluster. Fix: set KUBESRE_ADMIN_KEYS, " +
			"KUBESRE_OPERATOR_KEYS or KUBESRE_READONLY_KEYS (or DEMO_KEY_HMAC_SECRET), or unset REQUIRE_AUTH for local development")
	}
	return nil
}

// Migrate opens the database and applies every migration.
func Migrate(ctx context.Context, cfg *config.Config) (*store.DB, error) {
	db, err := store.Open(cfg)
	if err != nil {
		return nil, err
	}
	if err := db.Migrate(ctx, schema.All()); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// New builds the server. Nothing listens until Serve.
func New(ctx context.Context, cfg *config.Config) (*App, error) {
	slog.Info("kube-sre starting", "version", Version)
	if err := Check(cfg); err != nil {
		return nil, err
	}
	coord, err := llm.New(cfg, llm.Coordinator)
	if err != nil {
		return nil, err
	}
	sub, err := llm.New(cfg, llm.Subagent)
	if err != nil {
		return nil, err
	}
	slog.Info("llm provider", "provider", cfg.LLMProvider)

	db, err := Migrate(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}

	a := &App{Cfg: cfg, DB: db}
	// The recorder degrades gracefully: Start never fails, and events recorded
	// while it is down are written into the chain as a gap once it recovers.
	a.Recorder = recorder.New(db, cfg.FlightRecorder, cfg.RedactSecrets)
	a.Recorder.Start(ctx)
	a.Audit = audit.New(db)
	a.Audit.Start(ctx)
	a.Emitter = events.NewEmitter(a.Recorder)

	blocked := nsguard.Blocklist(cfg.BlockedNamespaces)
	tools := &agent.Toolset{
		Kubectl: kube.NewTool(kube.Config{
			Kubeconfig: cfg.KubeconfigPath, Timeout: cfg.KubectlTimeout, DestructiveTimeout: cfg.KubectlWriteTimeout,
			BlockedNamespaces: blocked, BlockedResources: cfg.BlockedResources, ErrorHints: cfg.ErrorHints, Recorder: a.Recorder,
		}),
		Helm: helm.New(blocked, cfg.BlockedResources),
		Prom: prom.New(cfg.PrometheusURL, blocked),
		Loki: loki.New(cfg.LokiURL, blocked),
	}
	resolver := cluster.NewResolver(cfg.ClusterID, cfg.KubeconfigPath)
	a.Agent = agent.New(agent.Deps{
		Cfg: cfg, Tools: tools, Coordinator: coord, Subagent: sub, Emitter: a.Emitter,
		Checkpoints: &agent.DBCheckpoints{DB: db},
		Snapshot:    &agent.Snapshotter{Bin: "kubectl", Kubeconfig: cfg.KubeconfigPath, Timeout: cfg.KubectlTimeout, Blocked: blocked},
		Playbooks:   playbooks.Load(),
		ClusterID:   resolver.Resolve,
	})
	a.Server = api.NewServer(cfg, a.Agent, a.Emitter, Version)
	a.Server.Audit = a.Audit
	a.Server.Health["audit"] = a.Audit.Status
	a.Server.Health["recorder"] = a.Recorder.Status
	return a, nil
}

// Serve accepts traffic until ctx ends, then shuts down in order.
func (a *App) Serve(ctx context.Context, addr string) error {
	// Everything above either succeeded or degraded on purpose, so accept traffic.
	a.Server.SetReady(true)
	slog.Info("listening", "addr", addr)
	err := a.Server.ListenAndServe(ctx, addr)
	a.Server.SetReady(false)
	slog.Info("kube-sre shutting down")
	a.Close()
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// Close stops background work and closes the database.
func (a *App) Close() {
	a.Recorder.Close()
	if a.DB != nil {
		a.DB.Close()
	}
}
