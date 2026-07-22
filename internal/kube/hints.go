package kube

import "regexp"

type hintRule struct {
	name string
	re   *regexp.Regexp
	hint string
}

func rule(name, pattern, hint string) hintRule {
	return hintRule{name, regexp.MustCompile("(?i)" + pattern), hint}
}

// Order matters: the first matching rule wins, so specific ones come first.
var hintRules = []hintRule{
	rule("namespace_not_found", `namespaces?\s+"[^"]+"\s+not\s+found`,
		"namespace does not exist, list them with `kubectl get ns`"),
	rule("container_not_found", `container\s+\S+\s+is\s+not\s+valid|container\s+not\s+found`,
		"pod has more than one container, pick one with `-c <name>`"),
	rule("pod_not_found", `pods?\s+"[^"]+"\s+not\s+found`,
		"the pod may have been replaced, list pods again for the new name"),
	rule("not_found", `\(NotFound\)|not found`,
		"not found, check the namespace (-n) and the exact name"),
	rule("forbidden", `\(Forbidden\)|forbidden:`,
		"permission denied, check the role bindings for this user"),
	rule("apiserver_unreachable",
		`connection refused|no route to host|i/o timeout|connection to the server .{0,80}? was refused`,
		"api server is unreachable, check the cluster and the kubeconfig"),
	rule("unable_to_connect", `unable to connect to the server`,
		"cluster is unreachable, the kubeconfig may be stale or the context wrong"),
	rule("missing_resource_type", `the server could not find the requested resource`,
		"that resource type is missing here, see `kubectl api-resources`"),
	rule("crd_not_recognized", `error: unable to recognize`,
		"the CRD is not installed, apply it before this resource"),
	rule("dns_lookup_failed", `dial tcp .* lookup`,
		"dns lookup for the api server failed, check resolv.conf or the VPN"),
	rule("method_not_allowed", `MethodNotAllowed`,
		"that verb is not allowed on this resource, see `kubectl explain`"),
	rule("immutable_field", `cannot patch .* immutable|field is immutable`,
		"field is immutable, replace or recreate the resource"),
	rule("etcd_recovering", `etcdserver:\s+(leader changed|request timed out)`,
		"etcd is recovering, retry in a few seconds"),
	rule("yaml_parse_error", `error converting YAML to JSON|yaml: unmarshal errors`,
		"yaml syntax error, check with `kubectl apply --dry-run=client`"),
	rule("concurrent_modification", `Operation cannot be fulfilled .* the object has been modified`,
		"the object changed while updating, fetch it again and retry"),
}

// Terminal rules are ones a retry cannot get past within one investigation.
var terminalRules = set("apiserver_unreachable", "unable_to_connect", "dns_lookup_failed")

// Interpret returns the matching rule name and hint for kubectl stderr.
func Interpret(stderr string) (name, hint string) {
	if stderr == "" {
		return "", ""
	}
	for _, r := range hintRules {
		if r.re.MatchString(stderr) {
			return r.name, r.hint
		}
	}
	return "", ""
}

func IsTerminal(name string) bool { return terminalRules[name] }

// Annotate appends a hint to the original error text, never replacing it.
func Annotate(stderr string) (string, string) {
	name, hint := Interpret(stderr)
	if hint == "" {
		return stderr, ""
	}
	return stderr + "\n-> " + hint, name
}
