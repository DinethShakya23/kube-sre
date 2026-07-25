package sensorium

import (
	"encoding/json"
	"fmt"
	"time"
)

// Observation is one normalised cluster signal.
//
//	pod_status  - status, watch_type, node, owner, uid, resource_version
//	event       - reason, message, involved_kind, event_type
//	node_status - status
//	metric      - promql, labels, value
type Observation struct {
	Kind      string
	ClusterID string
	Namespace string
	Name      string
	Fields    map[string]any
	TS        time.Time
}

// Str returns a string field, or "" when missing.
func (o Observation) Str(key string) string {
	s, _ := o.Fields[key].(string)
	return s
}

type condition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

type containerState struct {
	Waiting *struct {
		Reason string `json:"reason"`
	} `json:"waiting"`
	Terminated *struct {
		Reason   string `json:"reason"`
		Signal   int    `json:"signal"`
		ExitCode int    `json:"exitCode"`
	} `json:"terminated"`
	Running *struct{} `json:"running"`
}

type containerStatus struct {
	Ready bool           `json:"ready"`
	State containerState `json:"state"`
}

type podObject struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Namespace         string `json:"namespace"`
		Name              string `json:"name"`
		UID               string `json:"uid"`
		ResourceVersion   string `json:"resourceVersion"`
		DeletionTimestamp string `json:"deletionTimestamp"`
		OwnerReferences   []struct {
			Kind       string `json:"kind"`
			Name       string `json:"name"`
			Controller bool   `json:"controller"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
	Spec struct {
		NodeName       string            `json:"nodeName"`
		InitContainers []json.RawMessage `json:"initContainers"`
	} `json:"spec"`
	Status struct {
		Phase                 string            `json:"phase"`
		Reason                string            `json:"reason"`
		Conditions            []condition       `json:"conditions"`
		InitContainerStatuses []containerStatus `json:"initContainerStatuses"`
		ContainerStatuses     []containerStatus `json:"containerStatuses"`
	} `json:"status"`
}

func (p *podObject) condTrue(kind string) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == kind {
			return c.Status == "True"
		}
	}
	return false
}

// displayStatus computes the STATUS column the way `kubectl get pods` does.
// The order matters: phase, then status.reason, then init containers, then
// container states in reverse (the first container has the last word),
// then Completed vs Running/NotReady, and last the deletion override.
// Terminated reasons (OOMKilled, Error) must be reachable here, or a detector
// waiting for them can never fire.
func (p *podObject) displayStatus() string {
	st := &p.Status
	reason := st.Phase
	if reason == "" {
		reason = "Unknown"
	}
	if st.Reason != "" {
		reason = st.Reason
	}

	initializing := false
	for i, cs := range st.InitContainerStatuses {
		t, w := cs.State.Terminated, cs.State.Waiting
		if t != nil && t.ExitCode == 0 {
			continue
		}
		switch {
		case t != nil && t.Reason != "":
			reason = "Init:" + t.Reason
		case t != nil && t.Signal != 0:
			reason = fmt.Sprintf("Init:Signal:%d", t.Signal)
		case t != nil:
			reason = fmt.Sprintf("Init:ExitCode:%d", t.ExitCode)
		case w != nil && w.Reason != "" && w.Reason != "PodInitializing":
			reason = "Init:" + w.Reason
		default:
			total := len(p.Spec.InitContainers)
			if total == 0 {
				total = len(st.InitContainerStatuses)
			}
			reason = fmt.Sprintf("Init:%d/%d", i, total)
		}
		initializing = true
		break
	}

	if !initializing || p.condTrue("Initialized") {
		hasRunning := false
		for i := len(st.ContainerStatuses) - 1; i >= 0; i-- {
			cs := st.ContainerStatuses[i]
			t, w := cs.State.Terminated, cs.State.Waiting
			switch {
			case w != nil && w.Reason != "":
				reason = w.Reason
			case t != nil && t.Reason != "":
				reason = t.Reason
			case t != nil && t.Signal != 0:
				reason = fmt.Sprintf("Signal:%d", t.Signal)
			case t != nil:
				reason = fmt.Sprintf("ExitCode:%d", t.ExitCode)
			case cs.Ready && cs.State.Running != nil:
				hasRunning = true
			}
		}
		if reason == "Completed" && hasRunning {
			if p.condTrue("Ready") {
				reason = "Running"
			} else {
				reason = "NotReady"
			}
		}
	}

	if p.Metadata.DeletionTimestamp != "" {
		if st.Reason == "NodeLost" {
			return "Unknown"
		}
		if st.Phase != "Succeeded" && st.Phase != "Failed" {
			return "Terminating"
		}
	}
	return reason
}
