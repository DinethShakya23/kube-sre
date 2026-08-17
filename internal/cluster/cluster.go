// Package cluster resolves the identity of the cluster this process talks to.
//
// Every learned pattern is bound to the cluster it was seen on, so a Kind dev
// cluster does not pollute prompts on a production one. Strategies, in order:
//
//  1. CLUSTER_ID from configuration. Always wins.
//  2. The kubectl current context, hashed together with the API server URL.
//  3. The context alone, or the server URL hash alone.
//  4. The kube-system namespace UID, the conventional cluster identifier and the
//     only one available in-cluster (it needs cluster scoped read permission).
//  5. The literal "unknown".
//
// Read the sentinel correctly. Strategies 2 and 3 shell out to `kubectl config`,
// which needs a kubeconfig file, and an in-cluster deployment has none, so it
// resolves through strategy 4 or not at all. No read path filters the sentinel,
// deliberately: in a single cluster deployment every row is sentinel scoped and
// is that cluster's own data. The mitigation is to set CLUSTER_ID whenever
// several clusters share one database, not to drop rows on read.
package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Unresolved is what Resolve returns when nothing identifies the cluster.
const Unresolved = "unknown"

// IsResolved is false when the id is the sentinel, that is, when memory is not
// actually scoped to a cluster.
func IsResolved(id string) bool { return id != "" && id != Unresolved }

// Resolver works out the cluster id once and remembers it for the life of the
// process: cluster context is set at deploy time, and a restart picks up a change.
type Resolver struct {
	Configured string
	Kubeconfig string
	Bin        string

	once sync.Once
	id   string
}

func NewResolver(configured, kubeconfig string) *Resolver {
	return &Resolver{Configured: configured, Kubeconfig: kubeconfig, Bin: "kubectl"}
}

func (r *Resolver) kubectl(ctx context.Context, args ...string) string {
	kc := r.Kubeconfig
	if strings.HasPrefix(kc, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			kc = filepath.Join(home, kc[2:])
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, r.Bin, args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kc)
	out, err := cmd.Output()
	if err != nil {
		slog.Debug("cluster id probe failed", "args", strings.Join(args[:min(3, len(args))], " "), "err", err)
		return ""
	}
	return strings.TrimSpace(string(out))
}

func shortHash(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:n]
}

// Resolve returns a stable identifier for the cluster.
func (r *Resolver) Resolve(ctx context.Context) string {
	r.once.Do(func() { r.id = r.resolve(ctx) })
	return r.id
}

func (r *Resolver) resolve(ctx context.Context) string {
	if c := strings.TrimSpace(r.Configured); c != "" {
		return c
	}
	current := r.kubectl(ctx, "config", "current-context")
	// The server URL is a deterministic fallback when contexts are renamed or
	// absent. It is hashed to keep the id short and avoid leaking internal hostnames.
	server := r.kubectl(ctx, "config", "view", "--minify", "-o", "jsonpath={.clusters[0].cluster.server}")
	switch {
	case current != "" && server != "":
		return current + ":" + shortHash(server, 8)
	case current != "":
		return current
	case server != "":
		return "server:" + shortHash(server, 12)
	}
	// In cluster there is no kubeconfig, so ask the API. Reading kube-system's
	// immutable UID is not operating on the namespace: the protected namespace
	// rules govern what the agent may do, not this internal identity probe.
	if uid := r.kubectl(ctx, "get", "namespace", "kube-system", "-o", "jsonpath={.metadata.uid}"); uid != "" {
		return "uid:" + uid[:min(12, len(uid))]
	}
	slog.Warn("could not resolve the cluster identity, falling back to the sentinel. Memory, findings and learned "+
		"patterns are scoped to this id, so if more than one cluster writes to this database they will share one "+
		"scope. Set CLUSTER_ID to name this cluster explicitly.", "fallback", Unresolved)
	return Unresolved
}
