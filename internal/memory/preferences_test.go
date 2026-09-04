package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/config"
	"github.com/DinethShakya23/kube-sre/internal/store/storetest"
)

func prefStore(t *testing.T) *Store {
	t.Helper()
	db := storetest.New(t, StoreMigrations)
	s := NewStore(db, config.Load(func(string) string { return "" }))
	s.Now = func() time.Time { return t0 }
	return s
}

func TestExplicitPreferenceWinsOverInference(t *testing.T) {
	s := prefStore(t)
	ctx := context.Background()
	if !s.SetPreference(ctx, "u", "default_namespace", "prod", "explicit", nil) {
		t.Fatal("not stored")
	}
	c := 0.7
	s.SetPreference(ctx, "u", "default_namespace", "dev", "inferred", &c)
	got, err := s.RecallPreferences(ctx, "u", 10)
	if err != nil || len(got) != 1 || got[0].Value != "prod" || got[0].Source != "explicit" || got[0].Confidence != 1 {
		t.Errorf("%+v %v", got, err)
	}
	if got[0].Count != 2 {
		t.Errorf("the inferred write still counts: %d", got[0].Count)
	}
}

func TestInferredConfidenceGrowsToTheCap(t *testing.T) {
	s := prefStore(t)
	ctx := context.Background()
	c := 0.5
	for i := 0; i < 8; i++ {
		s.SetPreference(ctx, "u", "k", "v", "inferred", &c)
	}
	got, _ := s.RecallPreferences(ctx, "u", 10)
	if len(got) != 1 || got[0].Confidence != 0.95 {
		t.Errorf("%+v", got)
	}
}

func TestRecallOrderAndDecay(t *testing.T) {
	s := prefStore(t)
	ctx := context.Background()
	hi, lo := 0.9, 0.2
	s.SetPreference(ctx, "u", "b-inferred", "1", "inferred", &hi)
	s.SetPreference(ctx, "u", "a-explicit", "2", "explicit", nil)
	s.SetPreference(ctx, "u", "weak", "3", "inferred", &lo)
	got, _ := s.RecallPreferences(ctx, "u", 10)
	if len(got) != 2 || got[0].Key != "a-explicit" || got[1].Key != "b-inferred" {
		t.Errorf("%+v", got)
	}

	s.Now = func() time.Time { return t0.Add(200 * 24 * time.Hour) }
	got, _ = s.RecallPreferences(ctx, "u", 10)
	if len(got) != 1 || got[0].Key != "a-explicit" {
		t.Errorf("stale inferred should drop out: %+v", got)
	}
	if n := s.DecayAndForget(ctx); n != 1 { // only the weak one is below the floor
		t.Errorf("forgot %d", n)
	}
}

func TestPreferenceValueIsRedactedAndKeyBounded(t *testing.T) {
	s := prefStore(t)
	ctx := context.Background()
	s.SetPreference(ctx, "u", strings.Repeat("k", 300), "token=Bearer abcdef0123456789abcdef0123456789", "explicit", nil)
	got, _ := s.RecallPreferences(ctx, "u", 5)
	if len(got) != 1 || len(got[0].Key) != 120 || !strings.Contains(got[0].Value, "<redacted>") {
		t.Errorf("%+v", got)
	}
	if s.SetPreference(ctx, "", "k", "v", "explicit", nil) || s.SetPreference(ctx, "u", "  ", "v", "explicit", nil) {
		t.Error("empty user or key must be refused")
	}
}

func TestForgetPreferenceIsIdempotent(t *testing.T) {
	s := prefStore(t)
	ctx := context.Background()
	s.SetPreference(ctx, "u", "k", "v", "explicit", nil)
	if !s.ForgetPreference(ctx, "u", "k") || !s.ForgetPreference(ctx, "u", "k") {
		t.Error("forget")
	}
	if got, _ := s.RecallPreferences(ctx, "u", 5); len(got) != 0 {
		t.Errorf("%+v", got)
	}
}

func TestUnreadablePreferencesAreNotEmpty(t *testing.T) {
	s := prefStore(t)
	_, _ = s.DB.Exec(`DROP TABLE user_prefs`)
	got, err := s.RecallPreferences(context.Background(), "u", 5)
	if !errors.Is(err, ErrPrefsUnavailable) || got != nil {
		t.Errorf("%+v %v", got, err)
	}
}

func TestInferDefaultNamespace(t *testing.T) {
	s := prefStore(t)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		s.RecordOutcome(ctx, RCAOutcome{SessionID: "s", UserID: "focused", RootCause: "r", ClusterID: "c", Namespace: "payments"})
	}
	s.RecordOutcome(ctx, RCAOutcome{SessionID: "s", UserID: "focused", RootCause: "r", ClusterID: "c", Namespace: "other"})
	for _, ns := range []string{"a", "b", "c"} { // spread thin: no majority
		s.RecordOutcome(ctx, RCAOutcome{SessionID: "s", UserID: "spread", RootCause: "r", ClusterID: "c", Namespace: ns})
	}
	s.RecordOutcome(ctx, RCAOutcome{SessionID: "s", UserID: "few", RootCause: "r", ClusterID: "c", Namespace: "x"})

	if n := s.InferFromBehaviour(ctx); n != 1 {
		t.Fatalf("updated %d", n)
	}
	got, _ := s.RecallPreferences(ctx, "focused", 5)
	if len(got) != 1 || got[0].Key != "default_namespace" || got[0].Value != "payments" || got[0].Confidence != 0.8 {
		t.Errorf("%+v", got)
	}
	if got, _ := s.RecallPreferences(ctx, "spread", 5); len(got) != 0 {
		t.Errorf("no majority, no preference: %+v", got)
	}
}
