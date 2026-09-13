package api

import (
	"net/http"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/autonomy"
)

// v5Status is GET /v1/v5/status: the trust plane's live state in one read-only call.
// An operator needs to see which slices are active and whether the fail closed brakes
// are engaged, and this is the surface used to confirm a rollout, so it reports what
// is true: switches that do nothing under set_but_unwired_flags and not as active, and
// flags that are read by code inside a subsystem that is not running under
// degraded_experimental_flags.
func (s *Server) v5Status(w http.ResponseWriter, r *http.Request, _ string) {
	memory := map[string]any{}
	if f, ok := s.Health["memory"]; ok {
		memory = f()
	}
	state, _ := memory["state"].(string)
	cfg := s.Cfg
	engaged, freeze := false, cfg.V5ChangeFreeze
	if s.Budget != nil {
		engaged, freeze = s.Budget.KillSwitchEngaged(), s.Budget.ChangeFreezeActive(nil, nil)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"arm": "v1", "version": s.Version, "cortex_v5_enabled": cfg.CortexV5,
		"active_flags": cfg.ActiveFlags(), "set_but_unwired_flags": cfg.SetButUnwired(),
		"degraded_experimental_flags": cfg.DegradedFlags(state), "memory": memory,
		// Guard settings that parse cleanly and protect nothing: a blocked namespace
		// entry that cannot match any namespace, or an override the parser drops.
		"unenforceable_guard_config": nonNilStrings(cfg.Warnings()),
		"kill_switch_engaged":        engaged, "change_freeze": freeze, "spend_cap_usd": cfg.V5SpendCapUSD,
		"autonomy_promotion": s.autonomyPromotion(r),
	})
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// autonomyPromotion is the A3 statistical brake's live state, or why it is not
// acting. A read failure is reported as not operating with the error as the reason,
// the same distinction the watchtower makes when it decides whether to revoke. It must
// never answer "clean" for a store it could not read.
func (s *Server) autonomyPromotion(r *http.Request) map[string]any {
	if !s.Cfg.V5StatisticalPromotion {
		return autonomy.AutofixStatusUnavailable("flag off")
	}
	if s.Outcomes == nil {
		return autonomy.AutofixStatusUnavailable("no outcome store — the brake is not operating; A3 is governed by the allowlist alone")
	}
	st, err := s.Outcomes.AutofixStatus(r.Context(), float64(time.Now().Unix())/86400)
	if err != nil {
		return autonomy.AutofixStatusUnavailable("outcome store unreadable: " + err.Error())
	}
	return st
}
