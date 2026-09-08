package detect

import (
	"errors"
	"fmt"
	"regexp"
	"regexp/syntax"
	"strings"
)

// Liveness checks for a compiled predicate: can it ever fire on a real cluster?
//
// Compiling answers "is this a well formed predicate". It is not the question "can
// this predicate ever match an observation", and the gap between them is where dead
// detectors live. One shipped by hand: an anchored alternation with a space inside
// it compiles, loads, counts toward the detector total and passes the schema check,
// but an event reason never contains a space, so it was a permanent no-op. A kind
// the engine does not handle, a kind in the wrong case, and a Pod or Node predicate
// with no status regex fail the same way.
//
// That matters most on the natural language authoring path, where the predicate is
// written from prose by a model.
//
// EnumerateSamples expands a pattern into every string it can produce, every branch
// and not just one. Asserting that some string matches is useless: the string is
// generated from the pattern, so a stray space is in the language and the sample
// carries it. The useful question is whether every string it can produce is a value
// the cluster actually emits.

// SupportedKinds are the predicate kinds the engine handles, case sensitively.
var SupportedKinds = []string{"Pod", "Event", "Node"}

var (
	legalReason = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	legalStatus = regexp.MustCompile(`^[A-Za-z0-9:/._-]+$`)
)

const sampleCap = 500

// ErrUnsupportedPattern is a construct EnumerateSamples cannot expand. It is an
// error and not a harmless answer, so a caller that tolerates exotic patterns says so
// at the call site.
var ErrUnsupportedPattern = errors.New("pattern cannot be enumerated")

// EnumerateSamples returns every string the pattern can produce, or an error.
func EnumerateSamples(re *regexp.Regexp) ([]string, error) {
	parsed, err := syntax.Parse(re.String(), syntax.Perl)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupportedPattern, err)
	}
	return expand(parsed, re.String())
}

func productSize(parts [][]string) int {
	total := 1
	for _, p := range parts {
		n := len(p)
		if n < 1 {
			n = 1
		}
		total *= n
		if total > 1<<20 {
			return total
		}
	}
	return total
}

func expand(n *syntax.Regexp, pattern string) ([]string, error) {
	unsupported := func(what string) ([]string, error) {
		return nil, fmt.Errorf("%w: %s in %q", ErrUnsupportedPattern, what, pattern)
	}
	switch n.Op {
	case syntax.OpEmptyMatch, syntax.OpBeginLine, syntax.OpEndLine, syntax.OpBeginText, syntax.OpEndText,
		syntax.OpWordBoundary, syntax.OpNoWordBoundary:
		return []string{""}, nil // anchors contribute nothing
	case syntax.OpLiteral:
		return []string{string(n.Rune)}, nil
	case syntax.OpAnyCharNotNL, syntax.OpAnyChar:
		return []string{"x"}, nil
	case syntax.OpCharClass:
		var out []string
		for i := 0; i+1 < len(n.Rune); i += 2 {
			out = append(out, string(n.Rune[i])) // the ends of each range stand for it
			if n.Rune[i+1] != n.Rune[i] {
				out = append(out, string(n.Rune[i+1]))
			}
		}
		if len(out) == 0 {
			return unsupported("empty class")
		}
		return out, nil
	case syntax.OpCapture:
		return expand(n.Sub[0], pattern)
	case syntax.OpAlternate:
		var out []string
		for _, s := range n.Sub {
			part, err := expand(s, pattern)
			if err != nil {
				return nil, err
			}
			out = append(out, part...)
		}
		return out, nil
	case syntax.OpStar, syntax.OpPlus, syntax.OpQuest, syntax.OpRepeat:
		lo := 0
		switch n.Op {
		case syntax.OpPlus:
			lo = 1
		case syntax.OpRepeat:
			lo = n.Min
		}
		sub, err := expand(n.Sub[0], pattern)
		if err != nil {
			return nil, err
		}
		reps := lo
		if reps < 1 {
			reps = 1
		}
		var once []string
		for _, s := range sub {
			once = append(once, strings.Repeat(s, reps))
		}
		// An optional group can also contribute nothing, so check both worlds.
		if lo == 0 {
			once = append([]string{""}, once...)
		}
		return once, nil
	case syntax.OpConcat:
		var parts [][]string
		for _, s := range n.Sub {
			part, err := expand(s, pattern)
			if err != nil {
				return nil, err
			}
			parts = append(parts, part)
			if productSize(parts) > sampleCap {
				return unsupported(fmt.Sprintf("over %d samples", sampleCap))
			}
		}
		out := []string{""}
		for _, part := range parts {
			var next []string
			for _, prefix := range out {
				for _, s := range part {
					next = append(next, prefix+s)
				}
			}
			out = next
		}
		return out, nil
	}
	return unsupported(n.Op.String())
}

// PredicateLivenessErrors are the reasons pred can never match an observation; an
// empty list means it can fire. With strict false (the validator's setting) a
// pattern the enumerator cannot expand is unknown and not dead: refusing an
// author's valid but exotic regex would be worse than the gap this closes.
func PredicateLivenessErrors(p WatchPredicate, strict bool) ([]string, error) {
	supported := false
	for _, k := range SupportedKinds {
		supported = supported || k == p.Kind
	}
	if !supported {
		return []string{fmt.Sprintf("kind %q is never matched by the engine (it handles %s, case-sensitively), so this predicate can never fire",
			p.Kind, strings.Join(SupportedKinds, ", "))}, nil
	}
	var errs []string
	if (p.Kind == "Pod" || p.Kind == "Node") && p.StatusRegex == nil {
		errs = append(errs, fmt.Sprintf("a %s predicate without status_regex can never fire — matches() has nothing to test", p.Kind))
	}
	for _, f := range []struct {
		field string
		re    *regexp.Regexp
		legal *regexp.Regexp
	}{{"status", p.StatusRegex, legalStatus}, {"reason", p.ReasonRegex, legalReason}} {
		if f.re == nil {
			continue
		}
		samples, err := EnumerateSamples(f.re)
		if err != nil {
			if strict {
				return errs, err
			}
			continue
		}
		for _, s := range samples {
			if !f.legal.MatchString(s) {
				errs = append(errs, fmt.Sprintf("%s_regex %q can only be satisfied by %q, which is not a legal Kubernetes %s "+
					"(identifier-shaped, no spaces) — this predicate can never fire", f.field, f.re.String(), s, f.field))
				break
			}
		}
	}
	return errs, nil
}

// HealthyStatus is what the observer emits for an object in a normal steady state.
var HealthyStatus = map[string][]string{
	"Pod":  {"Running", "Completed", "Succeeded"},
	"Node": {"Ready"},
}

// PredicateHealthErrors are the reasons pred fires on objects that are fine. It is
// the mirror image of a dead predicate and went unrefused for as long: a dead
// predicate contributes silence, one that matches a healthy status contributes a
// finding about every object of its kind on the cluster, for ever. It is
// deliberately narrow and not a guess: it asks the predicate the same question the
// engine will, against the statuses the observer emits for a healthy object.
func PredicateHealthErrors(p WatchPredicate) []string {
	healthy, ok := HealthyStatus[p.Kind]
	if !ok || p.StatusRegex == nil {
		return nil
	}
	var hits []string
	for _, s := range healthy {
		if p.StatusRegex.MatchString(s) {
			hits = append(hits, s)
		}
	}
	if len(hits) == 0 {
		return nil
	}
	lower := strings.ToLower(p.Kind)
	return []string{fmt.Sprintf("status_regex %q matches %s, which is what the observer emits for a HEALTHY %s — this predicate fires on every "+
		"%s on the cluster, not on a fault. A %s predicate has no namespace or label scope, so there is no way to narrow it.",
		p.StatusRegex.String(), strings.Join(hits, ", "), p.Kind, lower, p.Kind)}
}

// Only strings that cannot plausibly be a real object name: the your-/your_ template
// form and the four templating syntaxes. Words like example, foo or test are not
// listed, since they are ordinary namespace and deployment names, and guessing at
// intent is how a validator starts lying.
var placeholderRe = regexp.MustCompile(`(?i)^(?:your[-_].*|<[^>]*>|\{\{.*\}\}|\$\{?[A-Za-z_][A-Za-z0-9_]*\}?|(?:CHANGE_?ME|REPLACE_?ME|PLACEHOLDER|TODO|FIXME))$`)

var labelMatcherRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*(=~|!~|!=|=)\s*"([^"]*)"`)

// ValidDirections are the two trend directions.
var ValidDirections = []string{"rising", "falling"}

// TrendLivenessErrors are the reasons a trend predicate can never fire. Every check
// is a provable impossibility read off the projection and its caller, which fire only
// when r2 >= min_r2 and 0 < eta <= min(horizon, fire_if_eta_within). Nothing guesses
// at whether a forecast is a good one, because that depends on runtime values.
func TrendLivenessErrors(t TrendPredicate) []string {
	metric := strings.TrimSpace(t.Metric)
	if metric == "" {
		return []string{"trend predicate has no metric — there is nothing to project"}
	}
	var errs []string
	for _, m := range labelMatcherRe.FindAllStringSubmatch(metric, -1) {
		if placeholderRe.MatchString(m[3]) {
			errs = append(errs, fmt.Sprintf("trend metric pins %s=%q, which is an unfilled template rather than a cluster object — the selector "+
				"matches no series, so this predicate can never fire (%q)", m[1], m[3], metric))
		}
	}
	if t.MinR2 > 1.0 {
		errs = append(errs, fmt.Sprintf("min_r2=%v is above 1.0 and r2 cannot exceed 1.0, so the fit check rejects every series — this predicate can never fire", t.MinR2))
	}
	for _, f := range []struct {
		name string
		v    int
	}{{"fire_if_eta_within_minutes", t.FireIfETAWithinMinutes}, {"projection_horizon_minutes", t.ProjectionHorizonMin}} {
		if f.v <= 0 {
			errs = append(errs, fmt.Sprintf("%s=%d excludes every projected ETA (the engine requires 0 < eta <= this) — this predicate can never fire", f.name, f.v))
		}
	}
	if t.WindowMinutes <= 0 {
		errs = append(errs, fmt.Sprintf("window_minutes=%d asks for an empty lookback, so the regression never gets the two samples it needs — this predicate can never fire", t.WindowMinutes))
	}
	// Not an impossibility, it is worse: anything that is not exactly "falling" is
	// read as rising, so a typo does not fail, it silently inverts the intent.
	if t.Direction != "rising" && t.Direction != "falling" {
		errs = append(errs, fmt.Sprintf("direction=%q is not one of %s; the engine would silently treat it as 'rising', which is the opposite condition "+
			"half the time — say which one you mean", t.Direction, strings.Join(ValidDirections, ", ")))
	}
	return errs
}
