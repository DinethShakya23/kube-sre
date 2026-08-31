package memory

import (
	"fmt"
	"sync"
)

// Observed memory behaviour: what memory DID, not what it is configured to do.
//
// "The pool is up" is a different question from "memory is working", and the
// distance between them is not hypothetical. An evaluation lane once ran nine
// hours with the memory state reporting ready while not one episode was ever
// written or recalled: the pool was fine, so health was green, and the numbers
// were graded before anyone noticed the subsystem under test had never run. The
// counters here are about outcomes: attempts, hits, failures, writes. A gauge that
// reads "enabled" cannot tell a working store from a dead one, but "asked 40 times,
// answered 0, wrote 0" is not ambiguous.

// AttemptsBeforeSuspicious is how many recall attempts to see before "asked
// repeatedly, never answered" is reportable. Below it an all miss run is a cold
// store, the normal state of a new cluster, and must not be dressed as a fault.
const AttemptsBeforeSuspicious = 10

// Liveness holds the counters.
type Liveness struct {
	mu              sync.Mutex
	recallAttempts  int
	recallHits      int
	recallFailures  int
	episodesWritten int

	// The last verdict from verifying the memory audit chain, recorded rather than
	// recomputed. Verifying reads every audit row, and the kubelet probes health every
	// few seconds, so a surface that re-verified on demand would make the probe's
	// latency a function of how much history the cluster has.
	chainChecks    int
	chainCheckedAt *float64
	chainValid     *bool
	chainVerified  *bool

	passFailures [][2]string
}

func NewLiveness() *Liveness { return &Liveness{} }

// RecordRecall notes one completed recall. hit means it returned an episode.
func (l *Liveness) RecordRecall(hit bool) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.recallAttempts++
	if hit {
		l.recallHits++
	}
	l.mu.Unlock()
}

// RecordRecallFailure notes a recall that failed instead of returning rows.
func (l *Liveness) RecordRecallFailure() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.recallAttempts++
	l.recallFailures++
	l.mu.Unlock()
}

// RecordEpisodeWritten notes one episode persisted.
func (l *Liveness) RecordEpisodeWritten() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.episodesWritten++
	l.mu.Unlock()
}

// Counters returns the four counters.
func (l *Liveness) Counters() map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return map[string]int{"recall_attempts": l.recallAttempts, "recall_hits": l.recallHits,
		"recall_failures": l.recallFailures, "episodes_written": l.episodesWritten}
}

// RecordChainCheck notes one completed verification of the audit chain. at is
// passed in, so the recorder is a pure function of its inputs.
func (l *Liveness) RecordChainCheck(valid, verified bool, at float64) {
	l.mu.Lock()
	l.chainChecks++
	l.chainCheckedAt, l.chainValid, l.chainVerified = &at, &valid, &verified
	l.mu.Unlock()
}

// ChainStatus is what the memory audit chain last said, and when, or why nothing
// said anything. state separates four things a boolean cannot:
//
//	off            the feature that writes the chain is disabled, so there is nothing to check
//	never-checked  enabled, but no verification has completed yet
//	unverified     a check ran and could not reach a conclusion (database unreachable, a head
//	               row it could not read). Deliberately NOT tampered: a detector that cries
//	               tamper whenever its own storage is down teaches operators to ignore it
//	intact/TAMPERED  a check ran and reached a conclusion
//
// A verdict older than staleAfter is reported as stale, not presented as current.
func (l *Liveness) ChainStatus(enabled bool, now, staleAfter float64) map[string]any {
	l.mu.Lock()
	checks, at, valid, verified := l.chainChecks, l.chainCheckedAt, l.chainValid, l.chainVerified
	l.mu.Unlock()
	state := "intact"
	switch {
	case !enabled:
		state = "off"
	case checks == 0:
		state = "never-checked"
	case valid != nil && !*valid:
		state = "TAMPERED"
	case verified == nil || !*verified:
		state = "unverified"
	}
	var age *float64
	if at != nil && now > 0 {
		a := now - *at
		if a < 0 {
			a = 0
		}
		age = &a
	}
	return map[string]any{
		"state": state, "checks": checks, "checked_at": at, "age_s": age, "valid": valid, "verified": verified,
		"stale": staleAfter > 0 && age != nil && *age > staleAfter,
	}
}

// Symptoms are plain statements about what is observably wrong, or none. They are
// phrased as observations, not diagnoses: each is a fact the process can prove
// about itself, and what it means needs context the process lacks (a brand new
// cluster legitimately recalls nothing). Empty is the healthy answer, and a
// non empty list is the machine readable form of "do not trust memory dependent
// results from this process".
func (l *Liveness) Symptoms(state string, observationsDropped int, chain map[string]any) []string {
	c := l.Counters()
	attempts, hits := c["recall_attempts"], c["recall_hits"]
	var out []string
	if state == "ready" && attempts >= AttemptsBeforeSuspicious {
		if hits == 0 {
			out = append(out, fmt.Sprintf("memory is connected and was queried %d times but has never returned an episode - recall is producing nothing", attempts))
		}
		if c["episodes_written"] == 0 {
			out = append(out, fmt.Sprintf("memory is connected and was queried %d times but no episode has ever been written - the store cannot fill, so recall can never improve", attempts))
		}
	}
	if c["recall_failures"] > 0 {
		out = append(out, fmt.Sprintf("%d of %d recall attempts failed outright", c["recall_failures"], attempts))
	}
	if observationsDropped > 0 {
		out = append(out, fmt.Sprintf("%d observations were dropped before reaching the knowledge graph", observationsDropped))
	}
	if chain != nil && chain["state"] == "TAMPERED" {
		// A performed check with a positive finding belongs here. "unverified" does
		// not: nobody looked is not evidence, and listing it would make an
		// unreachable database read as an integrity alarm.
		out = append(out, "the memory audit chain does not verify - its recorded rows no longer hash to what they carry, or the chain is shorter than its own head anchor says")
	}
	if chain != nil && chain["stale"] == true {
		age, _ := chain["age_s"].(*float64)
		out = append(out, fmt.Sprintf("the memory audit chain has not been verified for %.0fs - the last verdict is too old to describe the store as it is now", *age))
	}
	return out
}

// RecordPassFailure notes that a consolidation pass raised. Every pass returns an
// int, and 0 means both "it ran and there was nothing to do" and "it raised and its
// own guard caught it". Draining the failures makes that difference visible without
// changing the discipline that one failing pass must not stop the ones after it.
func (l *Liveness) RecordPassFailure(pass string, err error) {
	if l == nil {
		return
	}
	msg := err.Error()
	if r := []rune(msg); len(r) > 200 {
		msg = string(r[:200])
	}
	l.mu.Lock()
	l.passFailures = append(l.passFailures, [2]string{pass, msg})
	l.mu.Unlock()
}

// DrainPassFailures returns the failures since the last drain and forgets them, so
// the register is per pass rather than cumulative: a stale failure reported forever
// is its own kind of untrue signal.
func (l *Liveness) DrainPassFailures() [][2]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.passFailures
	l.passFailures = nil
	return out
}
