package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/agent"
	"github.com/DinethShakya23/kube-sre/internal/audit"
	"github.com/DinethShakya23/kube-sre/internal/config"
	"github.com/DinethShakya23/kube-sre/internal/detect"
	"github.com/DinethShakya23/kube-sre/internal/memory"
	"github.com/DinethShakya23/kube-sre/internal/nsguard"
	"github.com/DinethShakya23/kube-sre/internal/store/storetest"
)

func snapshotter(t *testing.T, body string) *agent.Snapshotter {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "kubectl")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &agent.Snapshotter{Bin: bin, Timeout: 5 * time.Second, Blocked: nsguard.Blocklist{}}
}

const podsHeader = `NAMESPACE NAME READY STATUS RESTARTS AGE\n`

func TestRecheckGradesTheClusterInsteadOfClosingBlind(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"healthy", `case "$2" in pods) printf '` + podsHeader + `shop web-1 1/1 Running 0 1d\n';; *) echo "No resources found";; esac`, memory.OutcomeResolved},
		{"broken", `case "$2" in pods) printf '` + podsHeader + `shop web-1 0/1 CrashLoopBackOff 9 1d\n';; *) echo "No resources found";; esac`, memory.OutcomeStillBroken},
		{"warnings but healthy pods", `case "$2" in pods) printf '` + podsHeader + `shop web-1 1/1 Running 0 1d\n';; *) printf 'LAST SEEN TYPE REASON OBJECT MESSAGE\n1m Warning BackOff pod/web-1 old\n';; esac`, memory.OutcomeResolved},
		{"unreadable cluster", `echo "connection refused" >&2; exit 1`, memory.OutcomeUnverified},
	}
	for _, c := range cases {
		d := recheckDispatch(snapshotter(t, c.body))
		if got := d(context.Background(), memory.Recheck{ID: 1, ClusterID: "c1", Namespace: "shop", Condition: "x"}, "A1"); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestRecheckAllNamespacesWhenNoneIsGiven(t *testing.T) {
	log := filepath.Join(t.TempDir(), "args")
	s := snapshotter(t, `echo "$*" >> `+log+`; printf '`+podsHeader+`a web 1/1 Running 0 1d\n'`)
	recheckDispatch(s)(context.Background(), memory.Recheck{Namespace: ""}, "A1")
	b, _ := os.ReadFile(log)
	if len(b) == 0 || !contains(string(b), "--all-namespaces") {
		t.Errorf("%s", b)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestScheduleRecheckIsGatedAndDeduped(t *testing.T) {
	db := storetest.New(t, memory.StoreMigrations, memory.EpisodeMigrations, memory.KGMigrations, memory.ProspectiveMigrations, audit.Migrations)
	f := detect.Finding{Playbook: "CrashLoopBackOff", Namespace: "shop", Object: "web-1"}
	cluster := func(context.Context) string { return "c1" }
	count := func() int {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM prospective_memory`).Scan(&n)
		return n
	}

	off := config.Load(func(string) string { return "" })
	scheduleRecheck(memory.NewStore(db, off), off, cluster, f)
	if count() != 0 {
		t.Error("MEMORY_PROSPECTIVE off must schedule nothing")
	}
	on := config.Load(func(k string) string { return map[string]string{"MEMORY_PROSPECTIVE": "true"}[k] })
	s := memory.NewStore(db, on)
	scheduleRecheck(s, on, cluster, f)
	scheduleRecheck(s, on, cluster, f)
	if count() != 1 {
		t.Errorf("a flapping fix refreshes one row: %d", count())
	}
	var cond, key string
	var dueAt float64
	_ = db.QueryRow(`SELECT condition, dedup_key, due_at FROM prospective_memory`).Scan(&cond, &key, &dueAt)
	if key != "recheck:CrashLoopBackOff:shop:web-1" || dueAt < float64(time.Now().Add(14*time.Minute).Unix()) {
		t.Errorf("%s %v", key, dueAt)
	}
	if !contains(cond, "'CrashLoopBackOff'") || !contains(cond, "'web-1'") {
		t.Errorf("%s", cond)
	}
}
