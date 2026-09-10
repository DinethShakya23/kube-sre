// Package change is the change ledger and the change first RCA prior.
//
// About 79% of outages follow a change, so the search prior should rank recent
// changes before any other hypothesis. The ledger captures the changes kube-sre
// itself applies (its mutating kubectl commands), which is reliable and needs no
// cluster watching. It is in process and bounded, a ring per cluster: the prior
// only needs recent changes, and a restart that loses them degrades to no prior.
package change

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Record is one change.
type Record struct {
	Kind      string  `json:"kind"`   // apply, scale, config, rollout, image, node, ...
	Target    string  `json:"target"` // deploy/web
	TS        float64 `json:"ts_epoch"`
	Namespace string  `json:"namespace"`
	Detail    string  `json:"detail"`
}

const maxPerCluster = 200

var verbKind = map[string]string{
	"apply": "apply", "create": "create", "delete": "delete", "scale": "scale",
	"patch": "config", "edit": "config", "replace": "config", "annotate": "config",
	"label": "config", "rollout": "rollout", "cordon": "node", "uncordon": "node",
	"drain": "node", "taint": "node",
}

var setSubs = map[string]bool{"image": true, "env": true, "resources": true, "serviceaccount": true, "selector": true}

// ParseKubectl turns a mutating kubectl command into a Record, or nil when it is not
// a recognised change.
func ParseKubectl(cmd string, ts float64, namespace string) *Record {
	toks := strings.Fields(strings.TrimSpace(cmd))
	if len(toks) > 0 && toks[0] == "kubectl" {
		toks = toks[1:]
	}
	if len(toks) == 0 {
		return nil
	}
	verb, rest := strings.ToLower(toks[0]), toks[1:]
	var kind string
	switch {
	case verb == "set" && len(rest) > 0:
		kind = "config"
		if sub := strings.ToLower(rest[0]); setSubs[sub] {
			kind = sub
		}
		rest = rest[1:]
	case verbKind[verb] != "":
		kind = verbKind[verb]
	default:
		return nil
	}
	target := ""
	for _, t := range rest {
		if !strings.HasPrefix(t, "-") {
			target = t
			break
		}
	}
	detail := strings.TrimSpace(cmd)
	if r := []rune(detail); len(r) > 120 {
		detail = string(r[:120])
	}
	return &Record{Kind: kind, Target: target, TS: ts, Namespace: namespace, Detail: detail}
}

// Ledger holds recent changes per cluster.
type Ledger struct {
	mu sync.Mutex
	by map[string][]Record
}

func NewLedger() *Ledger { return &Ledger{by: map[string][]Record{}} }

// Add records a change, dropping the oldest past the bound.
func (l *Ledger) Add(cluster string, r Record) {
	l.mu.Lock()
	defer l.mu.Unlock()
	xs := append(l.by[cluster], r)
	if len(xs) > maxPerCluster {
		xs = xs[len(xs)-maxPerCluster:]
	}
	l.by[cluster] = xs
}

// RecordCommands records every recognised mutating command and returns the count.
func (l *Ledger) RecordCommands(cluster string, commands []string, ts float64, namespace string) int {
	n := 0
	for _, c := range commands {
		if r := ParseKubectl(c, ts, namespace); r != nil {
			l.Add(cluster, *r)
			n++
		}
	}
	return n
}

// Recent returns a cluster's changes, limited to a namespace when one is given (a
// change with no namespace applies everywhere).
func (l *Ledger) Recent(cluster, namespace string) []Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Record
	for _, c := range l.by[cluster] {
		if namespace == "" || c.Namespace == "" || c.Namespace == namespace {
			out = append(out, c)
		}
	}
	return out
}

// RankByRecency puts the most recent change first: the search prior order.
func RankByRecency(changes []Record) []Record {
	out := append([]Record(nil), changes...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS > out[j].TS })
	return out
}

// RenderPrior is the "consider these changes first" prompt block, or "" when there
// are none. now, when non zero, adds each change's age.
func RenderPrior(changes []Record, maxItems int, now float64) string {
	ranked := RankByRecency(changes)
	if len(ranked) > maxItems {
		ranked = ranked[:maxItems]
	}
	if len(ranked) == 0 {
		return ""
	}
	lines := []string{"## Recent changes (consider these FIRST — ~79% of outages follow a change)"}
	for _, c := range ranked {
		ns, age, detail := "", "", ""
		if c.Namespace != "" {
			ns = " in " + c.Namespace
		}
		if now != 0 {
			mins := int((now - c.TS) / 60)
			if mins < 0 {
				mins = 0
			}
			age = fmt.Sprintf(" (%dm ago)", mins)
		}
		if c.Detail != "" {
			detail = " — " + c.Detail
		}
		lines = append(lines, fmt.Sprintf("- %s %s%s%s%s", c.Kind, c.Target, ns, age, detail))
	}
	return strings.Join(lines, "\n")
}
