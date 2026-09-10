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
	"github.com/DinethShakya23/kube-sre/internal/autonomy"
	"github.com/DinethShakya23/kube-sre/internal/change"
	"github.com/DinethShakya23/kube-sre/internal/cluster"
	"github.com/DinethShakya23/kube-sre/internal/config"
	"github.com/DinethShakya23/kube-sre/internal/detect"
	"github.com/DinethShakya23/kube-sre/internal/detectstore"
	"github.com/DinethShakya23/kube-sre/internal/digest"
	"github.com/DinethShakya23/kube-sre/internal/events"
	"github.com/DinethShakya23/kube-sre/internal/helm"
	"github.com/DinethShakya23/kube-sre/internal/kube"
	"github.com/DinethShakya23/kube-sre/internal/llm"
	"github.com/DinethShakya23/kube-sre/internal/loki"
	"github.com/DinethShakya23/kube-sre/internal/memory"
	"github.com/DinethShakya23/kube-sre/internal/nsguard"
	"github.com/DinethShakya23/kube-sre/internal/perception"
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
	Cfg        *config.Config
	DB         *store.DB
	Recorder   *recorder.Recorder
	Audit      *audit.Log
	Emitter    *events.Emitter
	Agent      *agent.Agent
	Server     *api.Server
	Perception *perception.Service
	Watchtower *autonomy.Watchtower
	Memory     *memory.Store
	// Consolidator runs the memory housekeeping passes.
	Consolidator *memory.Consolidator
	graph        *graphFeed
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
	a.Memory = memory.NewStore(db, cfg)
	if cfg.MemorySecurity {
		a.Memory.Guard = memory.NewGuard(db, cfg.MemoryWriteRate, cfg.MemoryTrustFloor)
	}
	a.graph = newGraphFeed(a.Memory, 1000)
	pb := playbooks.Load()
	a.Consolidator = &memory.Consolidator{Store: a.Memory, DetectFor: func(name string) ([]string, int, bool) {
		p := pb.Get(name)
		if p == nil || p.Detect == nil {
			return nil, 0, false
		}
		return p.Detect.PromQL, p.Detect.DebounceSeconds, true
	}}
	resolver := cluster.NewResolver(cfg.ClusterID, cfg.KubeconfigPath)
	snap := &agent.Snapshotter{Bin: "kubectl", Kubeconfig: cfg.KubeconfigPath, Timeout: cfg.KubectlTimeout, Blocked: blocked}
	ladder := autonomy.Ladder{Cfg: cfg}
	a.Consolidator.Extra = []memory.Pass{
		{Name: "prospective_fired", Run: func(ctx context.Context) int {
			return a.Memory.RunProspectiveOnce(ctx, ladder.LevelFor, func(l string) bool { return autonomy.AtLeast(l, "A1") }, recheckDispatch(snap))
		}},
		{Name: "rows_pruned", Run: a.Memory.PruneOnce},
	}
	ledger := change.NewLedger()
	a.Agent = agent.New(agent.Deps{
		Cfg: cfg, Tools: tools, Coordinator: coord, Subagent: sub, Emitter: a.Emitter,
		Checkpoints: &agent.DBCheckpoints{DB: db},
		Snapshot:    snap,
		Changes:     ledger,
		Writeback: func(ctx context.Context, cluster string, playbooks []string) {
			a.Memory.ApplyWriteback(ctx, cluster, memory.SignalsFromInvestigation(cluster, playbooks))
		},
		Playbooks: pb,
		Memory:    newMemoryAdapter(a.Memory),
		ClusterID: resolver.Resolve,
	})
	a.Perception = perception.NewService(cfg, pb)
	a.Perception.ClusterID = resolver.Resolve
	a.Perception.Recorder = a.Recorder
	a.Perception.Observe = a.graph.Offer
	// Findings open their own investigations through the same turn machinery chat uses.
	a.Watchtower = autonomy.NewWatchtower(cfg)
	a.Watchtower.Prepare = a.Emitter.Prepare
	a.Watchtower.AfterFix = func(f detect.Finding) { scheduleRecheck(a.Memory, cfg, resolver.Resolve, f) }
	a.Watchtower.Investigate = a.Agent.Run
	a.Perception.OnFinding = a.Watchtower.OnFinding
	a.Server = api.NewServer(cfg, a.Agent, a.Emitter, Version)
	a.Server.Perception = a.Perception
	a.Server.Health["sensorium"] = a.Perception.Status
	a.Server.Audit = a.Audit
	a.Server.Memory = a.Memory
	a.Server.Recorder = a.Recorder
	detectors := detectstore.New(db)
	a.Server.Detectors, a.Server.Compiler = detectors, sub
	a.Perception.StoredDetectors = detectors.Load
	report := digest.Builder{DB: db, Cfg: cfg, Perception: a.Perception.State}
	a.Server.Digest = &report
	a.Server.Postmortem = &digest.PostmortemBuilder{Builder: report, Recorder: a.Recorder, Narrator: sub}
	a.Server.Health["audit"] = a.Audit.Status
	a.Server.Health["recorder"] = a.Recorder.Status
	a.Server.Health["memory"] = a.memoryStatus
	return a, nil
}

// Serve accepts traffic until ctx ends, then shuts down in order.
func (a *App) Serve(ctx context.Context, addr string) error {
	// Perception failing must never cost availability: a start that raises is
	// recorded, and reported as an outage rather than a setting.
	a.Watchtower.Start(ctx)
	go a.graph.Run(ctx)
	go a.Consolidator.Loop(ctx, memory.ConsolidationInterval)
	if !a.Cfg.Sensorium {
		a.Perception.RecordDisabled()
	} else {
		// Off the startup path: working out the cluster identity shells out to kubectl,
		// which can take seconds when the cluster is unreachable, and the API must not
		// wait for that to accept traffic.
		go func() {
			if err := a.Perception.Start(ctx); err != nil {
				a.Perception.RecordStartFailure(err)
				slog.Warn("sensorium failed to start, continuing without", "err", err)
			}
		}()
	}
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
	a.Perception.Stop("", "")
	a.Watchtower.Wait()
	a.Recorder.Close()
	if a.DB != nil {
		a.DB.Close()
	}
}

func (a *App) memoryStatus() map[string]any {
	return map[string]any{
		"counters":            a.Memory.Live.Counters(),
		"graph_dropped":       a.graph.Dropped(),
		"graph_queue_backlog": len(a.graph.queue),
	}
}
