package fleet

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/DinethShakya23/kube-sre/internal/store/storetest"
)

func TestExchangeIsolatesTenantsAndFilters(t *testing.T) {
	x := NewExchange()
	x.Publish(Entry{"acme", "c1", "OOMKilled|payments", "raise limit"})
	x.Publish(Entry{"acme", "c2", "OOMKilled|payments", "raise limit to 1Gi"})
	x.Publish(Entry{"acme", "c2", "ImagePull|web", "fix tag"})
	x.Publish(Entry{"globex", "g1", "OOMKilled|payments", "their secret"})

	if got := x.Read("acme", "", ""); len(got) != 3 {
		t.Errorf("%+v", got)
	}
	for _, e := range x.Read("acme", "", "") {
		if e.Tenant != "acme" {
			t.Fatalf("a tenant read returned another tenant's row: %+v", e)
		}
	}
	if got := x.Read("acme", "c2", ""); len(got) != 1 || got[0].ClusterID != "c1" {
		t.Errorf("exclude self: %+v", got)
	}
	if got := x.Read("acme", "", "ImagePull|web"); len(got) != 1 {
		t.Errorf("signature: %+v", got)
	}
	if got := x.Read("nobody", "", ""); len(got) != 0 {
		t.Errorf("%+v", got)
	}
	if fmt.Sprint(x.Tenants()) != "[acme globex]" {
		t.Errorf("%v", x.Tenants())
	}
}

func TestExchangeIsBoundedPerTenant(t *testing.T) {
	x := NewExchange()
	for i := 0; i < maxPerTenant+20; i++ {
		x.Publish(Entry{"acme", "c1", "s", fmt.Sprint(i)})
	}
	x.Publish(Entry{"other", "c1", "s", "x"})
	got := x.Read("acme", "", "")
	if len(got) != maxPerTenant || got[0].Summary != "20" || len(x.Read("other", "", "")) != 1 {
		t.Errorf("%d %s", len(got), got[0].Summary)
	}
}

func TestPatternsNeedDistinctClustersAndNeverCrossTenants(t *testing.T) {
	sigs := []Signal{
		{"acme", "c1", "OOMKilled", "warning"}, {"acme", "c1", "OOMKilled", "warning"}, {"acme", "c2", "OOMKilled", "critical"}, {"acme", "c3", "OOMKilled", "warning"},
		{"acme", "c1", "gpu", "warning"}, {"acme", "c2", "gpu", "warning"},
		{"globex", "g1", "OOMKilled", "warning"}, {"globex", "g2", "OOMKilled", "warning"},
	}
	got := DetectPatterns(sigs, 3)
	if len(got) != 1 || got[0].Tenant != "acme" || got[0].Kind != "OOMKilled" || got[0].ClusterCount() != 3 || got[0].Severity != "critical" {
		t.Fatalf("one cluster repeating is not a fleet pattern, and tenants never mix: %+v", got)
	}
	if got := DetectPatterns(sigs, 2); len(got) != 3 || got[0].Kind != "OOMKilled" || got[0].Tenant != "acme" {
		t.Errorf("ordered by breadth: %+v", got)
	}
	if got := DetectPatterns(sigs, 4); len(got) != 0 {
		t.Errorf("%+v", got)
	}
}

func TestStoreIsTenantScopedAndWindowed(t *testing.T) {
	db := storetest.New(t, Migrations)
	now := 1_000_000.0
	s := &Store{DB: db, Now: func() float64 { return now }}
	ctx := context.Background()
	for _, e := range []Entry{{"acme", "c1", "sig", "one"}, {"acme", "c2", "sig", "two"}, {"globex", "g1", "sig", "secret"}} {
		if err := s.Publish(ctx, e); err != nil {
			t.Fatal(err)
		}
		now++
	}
	got, err := s.Read(ctx, "acme", "", "", 10)
	if err != nil || len(got) != 2 || got[0].Summary != "two" {
		t.Fatalf("%+v %v", got, err)
	}
	for _, e := range got {
		if strings.Contains(e.Summary, "secret") || e.Tenant != "acme" {
			t.Fatal("cross tenant leak")
		}
	}
	if got, _ = s.Read(ctx, "acme", "c2", "sig", 10); len(got) != 1 || got[0].ClusterID != "c1" {
		t.Errorf("%+v", got)
	}
	if got, _ = s.Read(ctx, "acme", "", "", 1); len(got) != 1 {
		t.Errorf("limit: %+v", got)
	}

	now = 2_000_000
	for _, c := range []string{"c1", "c2", "c3"} {
		_ = s.RecordSignal(ctx, Signal{Tenant: "acme", ClusterID: c, Kind: "OOMKilled"})
	}
	_ = s.RecordSignal(ctx, Signal{Tenant: "globex", ClusterID: "g1", Kind: "OOMKilled"})
	now += 100
	alerts, err := s.DetectFromStore(ctx, "acme", 3, 3600)
	if err != nil || len(alerts) != 1 || alerts[0].Severity != "warning" || alerts[0].ClusterCount() != 3 {
		t.Fatalf("%+v %v", alerts, err)
	}
	if alerts, _ = s.DetectFromStore(ctx, "globex", 3, 3600); len(alerts) != 0 {
		t.Errorf("%+v", alerts)
	}
	now += 7200
	if alerts, _ = s.DetectFromStore(ctx, "acme", 3, 3600); len(alerts) != 0 {
		t.Errorf("old signals fall out of the window: %+v", alerts)
	}
}
