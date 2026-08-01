// Package playbooks holds the built-in investigation playbooks.
//
// Each playbook is one YAML file in data/:
//
//	name: CrashLoopBackOff
//	detect: {...}            compiled zero-token detection (optional)
//	triggers: [...]          text matchers used when a snapshot is triaged
//	investigation_steps: []
//	expected_evidence: []
//	recommended_fix_template: text
package playbooks

import (
	"embed"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/DinethShakya23/kube-sre/internal/detect"
)

//go:embed data/*.yaml
var files embed.FS

// Trigger matches text from a snapshot. Matching ignores case.
type Trigger struct {
	PodStatus    *regexp.Regexp
	EventReason  *regexp.Regexp
	EventMessage *regexp.Regexp
}

type Playbook struct {
	Name               string
	Triggers           []Trigger
	InvestigationSteps []string
	ExpectedEvidence   []string
	FixTemplate        string
	// Detect is nil when the playbook is found by triage only.
	Detect *detect.DetectBlock
}

var triggerKeys = map[string]bool{"pod_status_regex": true, "event_reason_regex": true, "event_message_regex": true}

// Registry is the loaded, read-only set of playbooks.
type Registry struct {
	byName map[string]*Playbook
	order  []string
}

// Load reads every built-in playbook. A file that fails to load is logged and
// skipped so one bad file cannot take the rest down.
func Load() *Registry {
	r := &Registry{byName: map[string]*Playbook{}}
	entries, _ := files.ReadDir("data")
	for _, e := range entries {
		raw, err := files.ReadFile("data/" + e.Name())
		if err != nil {
			continue
		}
		pb, err := parse(raw)
		if err != nil {
			slog.Error("playbook load failed", "file", e.Name(), "err", err)
			continue
		}
		if _, dup := r.byName[pb.Name]; !dup {
			r.order = append(r.order, pb.Name)
		}
		r.byName[pb.Name] = pb
	}
	sort.Strings(r.order)
	slog.Info("playbooks loaded", "count", len(r.order))
	return r
}

func parse(raw []byte) (*Playbook, error) {
	var data map[string]any
	if err := yaml.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	name, _ := data["name"].(string)
	if name == "" {
		return nil, fmt.Errorf("missing name")
	}
	pb := &Playbook{Name: name}
	if t, present := data["triggers"]; present && t != nil {
		list, ok := t.([]any)
		if !ok {
			return nil, fmt.Errorf("triggers must be a list")
		}
		for _, item := range list {
			if m, ok := item.(map[string]any); ok {
				pb.Triggers = append(pb.Triggers, compileTrigger(m, name))
			}
		}
	}
	pb.InvestigationSteps = strings2(data["investigation_steps"])
	pb.ExpectedEvidence = strings2(data["expected_evidence"])
	if s, ok := data["recommended_fix_template"].(string); ok {
		pb.FixTemplate = strings.TrimSpace(s)
	}
	if d, ok := data["detect"].(map[string]any); ok {
		block, err := detect.ParseBlock(name, d)
		if err != nil {
			return nil, err
		}
		pb.Detect = block
	}
	return pb, nil
}

func strings2(v any) []string {
	var out []string
	list, _ := v.([]any)
	for _, x := range list {
		out = append(out, fmt.Sprint(x))
	}
	return out
}

// A trigger with an unknown key would load but never match, so say so loudly.
func compileTrigger(m map[string]any, playbook string) Trigger {
	for k := range m {
		if !triggerKeys[k] {
			slog.Warn("playbook trigger key is not recognised and is ignored, so it can never match",
				"playbook", playbook, "key", k)
		}
	}
	re := func(key string) *regexp.Regexp {
		s, _ := m[key].(string)
		if s == "" {
			return nil
		}
		c, err := regexp.Compile("(?i)" + s)
		if err != nil {
			slog.Warn("playbook trigger regex is invalid", "playbook", playbook, "key", key, "err", err)
			return nil
		}
		return c
	}
	return Trigger{PodStatus: re("pod_status_regex"), EventReason: re("event_reason_regex"), EventMessage: re("event_message_regex")}
}

// List returns playbooks in name order.
func (r *Registry) List() []*Playbook {
	out := make([]*Playbook, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.byName[n])
	}
	return out
}

func (r *Registry) Get(name string) *Playbook { return r.byName[name] }

// Detectors returns the compiled detect blocks, for the detector engine.
func (r *Registry) Detectors() []detect.DetectBlock {
	var out []detect.DetectBlock
	for _, pb := range r.List() {
		if pb.Detect != nil {
			out = append(out, *pb.Detect)
		}
	}
	return out
}

// Match returns the playbooks whose triggers match a snapshot. One matching
// trigger is enough.
func (r *Registry) Match(podsOut, eventsOut string) []string {
	var out []string
	for _, pb := range r.List() {
	next:
		for _, t := range pb.Triggers {
			switch {
			case t.PodStatus != nil && t.PodStatus.MatchString(podsOut),
				t.EventReason != nil && t.EventReason.MatchString(eventsOut),
				t.EventMessage != nil && t.EventMessage.MatchString(eventsOut):
				out = append(out, pb.Name)
				break next
			}
		}
	}
	return out
}
