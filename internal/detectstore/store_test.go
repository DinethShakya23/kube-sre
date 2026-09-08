package detectstore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/DinethShakya23/kube-sre/internal/llm"
	"github.com/DinethShakya23/kube-sre/internal/store/storetest"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	return New(storetest.New(t, Migrations))
}

func oom() map[string]any {
	return map[string]any{"watch_predicates": []any{map[string]any{"kind": "Pod", "status_regex": "^OOMKilled$"}}}
}

func TestValidateAcceptsAGoodBlock(t *testing.T) {
	b, errs := Validate(oom(), "nl:oom")
	if b == nil || len(errs) != 0 || b.Playbook != "nl:oom" {
		t.Errorf("%+v %v", b, errs)
	}
}

func TestValidateRefusesWhatCompilesButCannotWork(t *testing.T) {
	cases := map[string]struct {
		raw  map[string]any
		want string
	}{
		"not a block":       {map[string]any{"promql": []any{"up"}}, "no valid predicates"},
		"bad regex":         {map[string]any{"watch_predicates": []any{map[string]any{"kind": "Pod", "status_regex": "("}}}, "invalid predicate"},
		"dead (space)":      {map[string]any{"watch_predicates": []any{map[string]any{"kind": "Event", "reason_regex": "^(A | B)$"}}}, "can never fire"},
		"healthy pod":       {map[string]any{"watch_predicates": []any{map[string]any{"kind": "Pod", "status_regex": "^Running$"}}}, "HEALTHY Pod"},
		"template selector": {map[string]any{"trend_predicates": []any{map[string]any{"metric": `x{deployment="your-deployment-name"}`, "threshold": 1}}}, "unfilled template"},
		"bad direction":     {map[string]any{"trend_predicates": []any{map[string]any{"metric": "x", "threshold": 1, "direction": "sideways"}}}, "silently treat it as 'rising'"},
		"wrong kind case":   {map[string]any{"watch_predicates": []any{map[string]any{"kind": "pod", "status_regex": "^OOMKilled$"}}}, "case-sensitively"},
	}
	for name, c := range cases {
		b, errs := Validate(c.raw, "nl")
		if b != nil || len(errs) == 0 || !strings.Contains(strings.Join(errs, " "), c.want) {
			t.Errorf("%s: %+v %v", name, b, errs)
		}
	}
	if b, errs := Validate(nil, "nl"); b != nil || len(errs) != 1 {
		t.Errorf("nil: %v", errs)
	}
}

func TestStageListPromoteDemote(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if !s.Stage(ctx, "nl:oom", "pods killed for memory", oom(), "operator", "global") {
		t.Fatal("not staged")
	}
	if s.Stage(ctx, "nl:oom", "again", oom(), "operator", "global") {
		t.Error("a duplicate name must not stage twice")
	}
	rows, err := s.List(ctx, "", "global")
	if err != nil || len(rows) != 1 || rows[0].Status != "shadow" || rows[0].Source != "nl" || rows[0].ReviewedBy == nil || *rows[0].ReviewedBy != "operator" ||
		rows[0].CreatedFrom == nil || *rows[0].CreatedFrom != "pods killed for memory" {
		t.Fatalf("%+v %v", rows, err)
	}
	if got, _ := s.List(ctx, "active", "global"); len(got) != 0 {
		t.Errorf("filter: %+v", got)
	}

	if ok, err := s.Promote(ctx, "nl:oom", "admin", "global"); !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	if got, _ := s.List(ctx, "active", "global"); len(got) != 1 || *got[0].ReviewedBy != "admin" {
		t.Errorf("%+v", got)
	}
	if !s.Demote(ctx, "nl:oom", "admin", "global") {
		t.Error("demote")
	}
	if ok, err := s.Promote(ctx, "missing", "admin", "global"); ok || err != nil {
		t.Errorf("unknown name is not found: %v %v", ok, err)
	}
	if s.Demote(ctx, "missing", "admin", "global") {
		t.Error("unknown name")
	}
}

func TestPromotionOfADeadDetectorIsRefused(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	dead := map[string]any{"watch_predicates": []any{map[string]any{"kind": "Event", "reason_regex": "^(A | B)$"}}}
	s.Stage(ctx, "nl:dead", "x", dead, "op", "global")
	ok, err := s.Promote(ctx, "nl:dead", "admin", "global")
	if ok || !errors.Is(err, ErrCannotFire) {
		t.Fatalf("%v %v", ok, err)
	}
	// Asked from a cluster that sets its own id: the global row must still be checked.
	if ok, err := s.Promote(ctx, "nl:dead", "admin", "prod-1"); ok || !errors.Is(err, ErrCannotFire) {
		t.Errorf("the gate must see global rows from a named cluster: %v %v", ok, err)
	}
	if rows, _ := s.List(ctx, "shadow", "global"); len(rows) != 1 {
		t.Error("a refused promotion must leave the row alone")
	}
}

func TestListReportsAnUnreadableStore(t *testing.T) {
	s := newStore(t)
	_, _ = s.DB.Exec(`DROP TABLE detectors`)
	rows, err := s.List(context.Background(), "", "global")
	if !errors.Is(err, ErrUnavailable) || rows != nil {
		t.Errorf("%v %v", rows, err)
	}
	if _, _, err := s.Load(context.Background(), "c"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("load must not return empty sets on a failed read: %v", err)
	}
}

func TestLoadSplitsActiveAndShadowAndReadsGlobalRows(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	s.Stage(ctx, "nl:a", "d", oom(), "op", "global")
	s.Stage(ctx, "nl:b", "d", oom(), "op", "global")
	s.Stage(ctx, "nl:other", "d", oom(), "op", "someone-else")
	_, _ = s.Promote(ctx, "nl:a", "admin", "global")
	_, _ = s.Promote(ctx, "nl:other", "admin", "someone-else")

	active, shadow, err := s.Load(ctx, "prod-1")
	if err != nil || len(active) != 1 || active[0].Playbook != "nl:a" || len(shadow) != 1 || shadow[0].Playbook != "nl:b" {
		t.Fatalf("%+v %+v %v", active, shadow, err)
	}
	s.Demote(ctx, "nl:b", "admin", "global")
	if _, shadow, _ = s.Load(ctx, "prod-1"); len(shadow) != 0 {
		t.Errorf("demoted detectors do not load: %+v", shadow)
	}
}

func TestLoadDropsDeadPredicatesButKeepsTheLiveOnes(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mixed := map[string]any{
		"watch_predicates": []any{map[string]any{"kind": "Pod", "status_regex": "^Running$"}},
		"trend_predicates": []any{map[string]any{"metric": `kube_deployment_status_replicas{deployment="your-deployment-name"}`, "threshold": 1}},
	}
	s.Stage(ctx, "nl:mixed", "d", mixed, "op", "global")
	_, shadow, err := s.Load(ctx, "c")
	if err != nil || len(shadow) != 1 {
		t.Fatalf("%v %+v", err, shadow)
	}
	b := shadow[0]
	if len(b.TrendPredicates) != 0 || len(b.DroppedPredicates) != 1 || !strings.Contains(b.DroppedPredicates[0], "unfilled template") {
		t.Errorf("dead trend predicate: %+v", b)
	}
	// The healthy-object predicate is live: it is loaded and named, never deleted.
	if len(b.WatchPredicates) != 1 || len(b.FiresOnHealthy) != 1 {
		t.Errorf("fires-on-healthy: %+v", b)
	}
}

func TestLoadSkipsWhatIsNotADetectorAndNeverFiresDeadRows(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_, _ = s.DB.Exec(s.DB.Q(`INSERT INTO detectors (cluster_id, name, source, predicate, status) VALUES ('global', 'learned', 'learned', '{"pattern":"x"}', 'active')`))
	s.Stage(ctx, "nl:dead", "d", map[string]any{"watch_predicates": []any{map[string]any{"kind": "Pod"}}}, "op", "global")
	active, shadow, err := s.Load(ctx, "c")
	if err != nil || len(active) != 0 || len(shadow) != 0 {
		t.Errorf("%+v %+v %v", active, shadow, err)
	}
}

type reply string

func (r reply) Chat(_ context.Context, msgs []llm.Message, _ []llm.ToolSpec, _ llm.Options) (*llm.Response, error) {
	if msgs[0].Role != llm.System || !strings.Contains(msgs[0].Content, "NEVER leave a placeholder") {
		panic("the authoring rules are not in the prompt")
	}
	return &llm.Response{Message: llm.Message{Content: string(r)}}, nil
}

type failing struct{}

func (failing) Chat(context.Context, []llm.Message, []llm.ToolSpec, llm.Options) (*llm.Response, error) {
	return nil, errors.New("model down")
}

func TestCompileParsesJSONAndFailsOpen(t *testing.T) {
	got := Compile(context.Background(), reply("Sure!\n```json\n{\"watch_predicates\":[{\"kind\":\"Pod\",\"status_regex\":\"^OOMKilled$\"}]}\n```"), "oom")
	if _, ok := got["watch_predicates"]; !ok {
		t.Errorf("%v", got)
	}
	for _, m := range []llm.Model{reply("no json here"), reply("{broken"), reply("[1,2]"), failing{}, nil} {
		if got := Compile(context.Background(), m, "x"); len(got) != 0 {
			t.Errorf("%T: %v", m, got)
		}
	}
}

func TestEvaluateOnlyFlagsForReview(t *testing.T) {
	many := make([]bool, 30)
	for i := range many {
		many[i] = true
	}
	if v := Evaluate(many, 20, 0.85); !v.ReadyForReview || v.Firings != 30 || len(v.Reasons) != 0 {
		t.Errorf("%+v", v)
	}
	if v := Evaluate(many[:10], 20, 0.85); v.ReadyForReview || !strings.Contains(v.Reasons[0], "only 10 shadow firings") {
		t.Errorf("%+v", v)
	}
	noisy := append([]bool{false, false, false, false, false, false}, many[:24]...)
	if v := Evaluate(noisy, 20, 0.9); v.ReadyForReview || !strings.Contains(strings.Join(v.Reasons, ";"), "precision LCB") {
		t.Errorf("%+v", v)
	}
	if v := Evaluate(nil, 20, 0.9); v.ReadyForReview || v.PrecisionLCB != 0 || len(v.Reasons) != 2 {
		t.Errorf("%+v", v)
	}
}
