package kube

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// Flags that name a manifest the tool never sees. `-f -` is the supported form:
// the YAML arrives as the stdin argument, is validated, is read by the protected
// access checks and is shown to the human in the approval prompt. A path or a URL
// is none of those things, because kubectl fetches it after approval.
var manifestSourceFlags = set("-f", "--filename", "-k", "--kustomize")

// -f means --follow here, not --filename.
var followVerbs = set("logs")

// externalManifestSource returns the value of a -f or -k that is not stdin, or
// nil when every manifest source is `-`. It accepts every spelling pflag does
// (-f x, -f=x, -fx, --filename x, --filename=x), because a guard that reads one
// spelling of a flag guards one spelling of a command.
func externalManifestSource(verb string, args []string) *string {
	for i := 1; i < len(args); i++ {
		tok := args[i]
		name, value, has := tok, "", false
		switch {
		case strings.HasPrefix(tok, "-") && strings.Contains(tok, "="):
			name, value, _ = strings.Cut(tok, "=")
			has = true
		case !strings.HasPrefix(tok, "--") && len(tok) > 2 && (tok[:2] == "-f" || tok[:2] == "-k"):
			name, value, has = tok[:2], tok[2:], true
		}
		if manifestSourceFlags[name] && !((name == "-f" || name == "--filename") && followVerbs[verb]) {
			if !has {
				if i+1 < len(args) {
					value = args[i+1]
				}
				i++
			}
			if value != "-" {
				return &value
			}
		}
	}
	return nil
}

// decodeAll reads every document in a YAML stream.
func decodeAll(stdin string) ([]any, error) {
	dec := yaml.NewDecoder(strings.NewReader(stdin))
	var docs []any
	for {
		var doc any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return docs, err
		}
		docs = append(docs, doc)
	}
}

// ValidateStdinYAML refuses stdin that is not YAML, or is empty or null.
func ValidateStdinYAML(stdin string) error {
	docs, err := decodeAll(stdin)
	if err != nil {
		return fmt.Errorf("Invalid YAML in stdin: %v", err)
	}
	if len(docs) == 0 || (len(docs) == 1 && docs[0] == nil) {
		return errors.New("stdin YAML is empty or null")
	}
	return nil
}

// manifestDocs returns every mapping document in a stdin manifest, including the
// items of a `kind: List`. `kubectl apply -f -` names no resource and no
// namespace on the command line, so everything that reads the command is blind to
// what it targets. Parsing is best effort: ValidateStdinYAML has already refused
// bad input, and a shape we do not understand only means the argv checks still apply.
func manifestDocs(stdin string) []map[string]any {
	if stdin == "" {
		return nil
	}
	docs, err := decodeAll(stdin)
	if err != nil {
		return nil
	}
	var out, stack []map[string]any
	for _, d := range docs {
		if m, ok := d.(map[string]any); ok {
			stack = append(stack, m)
		}
	}
	for len(stack) > 0 {
		doc := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		out = append(out, doc)
		if items, ok := doc["items"].([]any); ok {
			for _, it := range items {
				if m, ok := it.(map[string]any); ok {
					stack = append(stack, m)
				}
			}
		}
	}
	return out
}

func manifestKinds(stdin string) map[string]bool {
	out := map[string]bool{}
	for _, d := range manifestDocs(stdin) {
		if k, ok := d["kind"]; ok && k != nil && fmt.Sprint(k) != "" {
			out[fmt.Sprint(k)] = true
		}
	}
	return out
}

// manifestNamespaces returns every metadata.namespace named in a stdin manifest.
// It does not walk into a Pod spec's volumes or envFrom: mounting a Secret is
// what a Pod is for, and blocking it would break ordinary deployments. The
// boundary is what the manifest is and where it goes.
func manifestNamespaces(stdin string) map[string]bool {
	out := map[string]bool{}
	for _, d := range manifestDocs(stdin) {
		if meta, ok := d["metadata"].(map[string]any); ok {
			if ns, ok := meta["namespace"]; ok && ns != nil && fmt.Sprint(ns) != "" {
				out[strings.ToLower(fmt.Sprint(ns))] = true
			}
		}
	}
	return out
}

func yamlDump(v any) string {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	_ = enc.Encode(v)
	_ = enc.Close()
	return b.String()
}
