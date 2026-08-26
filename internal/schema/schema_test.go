package schema

import (
	"context"
	"testing"

	"github.com/DinethShakya23/kube-sre/internal/store/storetest"
)

func TestVersionsAreUniqueAndApplyCleanly(t *testing.T) {
	seen := map[int]string{}
	for _, m := range All() {
		if prev, dup := seen[m.Version]; dup {
			t.Errorf("version %d used by %q and %q", m.Version, prev, m.Name)
		}
		seen[m.Version] = m.Name
	}
	db := storetest.New(t, All())
	v, err := db.Version(context.Background())
	if err != nil || v != Latest() {
		t.Errorf("applied %d, latest %d, err %v", v, Latest(), err)
	}
	// applying again is a no-op
	if err := db.Migrate(context.Background(), All()); err != nil {
		t.Errorf("idempotent: %v", err)
	}
}
