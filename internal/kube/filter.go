package kube

import (
	"bytes"
	"encoding/json"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/DinethShakya23/kube-sre/internal/nsguard"
	"github.com/DinethShakya23/kube-sre/internal/policy"
)

// kubectl's fixed shape outputs: the only ones whose layout kubectl decides and
// that can therefore be filtered. `name` and `jsonpath` are handled separately.
var fixedShapeFormats = set("", "wide", "json", "yaml")

func noting(text string, dropped int) string {
	if dropped > 0 {
		return text + nsguard.WithheldNote(dropped, "namespace")
	}
	return text
}

// unfilterableFormat refuses an output format whose shape the caller chose.
// custom-columns, go-template and jsonpath render whatever was asked for in
// whatever order, so there is no column or path the filter can rely on and the
// row of a protected namespace is indistinguishable from any other.
func unfilterableFormat(format, listing string) string {
	return "[Protected] '-o " + format + "' renders the output you choose, so results from " +
		"infrastructure namespaces cannot be separated out of " + listing + ". Re-run with " +
		"-o json (filtered per item), or narrow the query with -n <namespace>."
}

func decodeStructured(output, format string) (map[string]any, bool) {
	var doc map[string]any
	if format == "json" {
		dec := json.NewDecoder(strings.NewReader(output))
		dec.UseNumber()
		if dec.Decode(&doc) != nil {
			return nil, false
		}
	} else if yaml.Unmarshal([]byte(output), &doc) != nil {
		return nil, false
	}
	if doc == nil {
		return nil, false
	}
	if _, ok := doc["items"].([]any); !ok {
		return nil, false
	}
	return doc, true
}

func encodeStructured(doc map[string]any, format string) string {
	if format == "json" {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		_ = enc.Encode(doc)
		return strings.TrimRight(b.String(), "\n")
	}
	return yamlDump(doc)
}

func metaString(item any, field string) string {
	m, ok := item.(map[string]any)
	if !ok {
		return ""
	}
	meta, _ := m["metadata"].(map[string]any)
	v, ok := meta[field]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.ToLower(s)
	}
	return ""
}

// filterStructuredNamespaces drops blocked namespaces from a -o json or -o yaml
// namespace listing. It fails closed: a payload that cannot be parsed is
// replaced, never returned unfiltered.
func filterStructuredNamespaces(output, format string, blocked nsguard.Blocklist) string {
	doc, ok := decodeStructured(output, format)
	if !ok {
		return "[Protected] This namespace listing could not be parsed, so blocked namespaces " +
			"could not be stripped from it. Re-run without -o json/-o yaml."
	}
	items := doc["items"].([]any)
	var kept []any
	for _, it := range items {
		m, isMap := it.(map[string]any)
		if isMap {
			if blocked[metaString(m, "name")] {
				continue
			}
		}
		kept = append(kept, it)
	}
	if kept == nil {
		kept = []any{}
	}
	doc["items"] = kept
	nsguard.Annotate(doc, len(items)-len(kept), "namespace")
	return encodeStructured(doc, format)
}

// splitBlocks cuts describe output into blocks that start at `Name:`.
func splitBlocks(output string) [][]string {
	var blocks [][]string
	for _, line := range policy.SplitLines(output) {
		if strings.HasPrefix(line, "Name:") || len(blocks) == 0 {
			blocks = append(blocks, nil)
		}
		blocks[len(blocks)-1] = append(blocks[len(blocks)-1], line)
	}
	return blocks
}

// filterDescribedNamespaces drops the blocks of `describe namespaces` that
// describe blocked ones. Each entry starts with `Name:` at column zero and every
// nested line is indented, so that is a reliable block boundary.
func filterDescribedNamespaces(output string, blocked nsguard.Blocklist) string {
	blocks := splitBlocks(output)
	var kept strings.Builder
	keptCount := 0
	for _, block := range blocks {
		name := ""
		if strings.HasPrefix(block[0], "Name:") {
			name = strings.ToLower(strings.TrimSpace(strings.SplitN(block[0], ":", 2)[1]))
		}
		if !blocked[name] {
			kept.WriteString(strings.Join(block, ""))
			keptCount++
		}
	}
	return noting(kept.String(), len(blocks)-keptCount)
}

// FilterNamespaceOutput strips blocked namespaces from a namespace listing in
// every output format. A bare `kubectl get namespaces` is allowed on purpose, so
// an agent can see the shape of the cluster, and the blocked ones are removed
// from the answer instead.
func FilterNamespaceOutput(verb string, args []string, output string, blocked nsguard.Blocklist) string {
	if !namespaceKinds[extractResourceType(verb, args)] {
		return output
	}
	format := FlagValue(args, "-o", "--output", verb)

	if verb == "describe" {
		return filterDescribedNamespaces(output, blocked)
	}
	lines := policy.SplitLines(output)

	switch {
	case format == "name":
		var kept strings.Builder
		n := 0
		for _, l := range lines {
			seg := strings.Split(strings.TrimSpace(l), "/")
			if !blocked[strings.ToLower(seg[len(seg)-1])] {
				kept.WriteString(l)
				n++
			}
		}
		return noting(kept.String(), len(lines)-n)
	case format == "json" || format == "yaml":
		return filterStructuredNamespaces(output, format, blocked)
	case !fixedShapeFormats[format]:
		// jsonpath was once filtered by dropping whitespace separated tokens equal
		// to a blocked name. That works for exactly one jsonpath, and three ordinary
		// ones defeated it with no withheld note. jsonpath renders whatever the
		// caller asked for, exactly like custom-columns and go-template.
		return unfilterableFormat(format, "a namespace listing")
	}
	// The default table: keep the header and rows whose first column is not blocked.
	var result strings.Builder
	dropped := 0
	for _, line := range lines {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] == "NAME" || !blocked[strings.ToLower(f[0])] {
			result.WriteString(line)
		} else {
			dropped++
		}
	}
	return noting(result.String(), dropped)
}

// FilterAllNamespacesOutput drops rows of blocked namespaces from a cluster wide
// listing. `get pods -n kube-system` is refused, so `get pods -A` must not return
// the same rows. Every cluster wide output carries the namespace, as the first
// table column, as metadata.namespace or as a `Namespace:` field, except -o name
// and -o jsonpath, whose shape the caller chose. Those are refused rather than
// passed through, the same fail closed choice the structured filter makes.
func FilterAllNamespacesOutput(verb string, args []string, output string, blocked nsguard.Blocklist) string {
	if !isAllNamespaces(args) {
		return output
	}
	format := FlagValue(args, "-o", "--output", verb)

	if format == "name" || (format != "" && strings.Contains(format, "jsonpath")) {
		return "[Protected] '-o " + format + "' with --all-namespaces carries no namespace, so " +
			"results from infrastructure namespaces cannot be separated out. Re-run without " +
			"-o (the table lists NAMESPACE), or narrow the query with -n <namespace>."
	}
	if format == "json" || format == "yaml" {
		return filterStructuredByNamespace(output, format, blocked)
	}
	if verb == "describe" {
		return filterDescribedByNamespace(output, blocked)
	}
	if !fixedShapeFormats[format] {
		return unfilterableFormat(format, "a cluster-wide listing")
	}
	result, dropped := blocked.DropTableRows(output)
	if dropped > 0 {
		return result + nsguard.WithheldNote(dropped, "")
	}
	return result
}

func filterStructuredByNamespace(output, format string, blocked nsguard.Blocklist) string {
	doc, ok := decodeStructured(output, format)
	if !ok {
		return "[Protected] The cluster-wide result could not be parsed, so results from " +
			"infrastructure namespaces could not be separated out. Narrow the query with " +
			"-n <namespace>."
	}
	items := doc["items"].([]any)
	kept := make([]any, 0, len(items))
	for _, it := range items {
		if !blocked[metaString(it, "namespace")] {
			kept = append(kept, it)
		}
	}
	dropped := len(items) - len(kept)
	if dropped == 0 {
		return output
	}
	doc["items"] = kept
	// The notice goes inside the document. Appended after it, the output would no
	// longer parse: the one filter that told the truth broke the format it told it in.
	nsguard.Annotate(doc, dropped, "")
	return encodeStructured(doc, format)
}

func filterDescribedByNamespace(output string, blocked nsguard.Blocklist) string {
	var blocks [][]string
	var current []string
	for _, line := range policy.SplitLines(output) {
		if strings.HasPrefix(line, "Name:") && len(current) > 0 {
			blocks = append(blocks, current)
			current = nil
		}
		current = append(current, line)
	}
	if len(current) > 0 {
		blocks = append(blocks, current)
	}
	var kept strings.Builder
	dropped := 0
	for _, block := range blocks {
		ns := ""
		for _, line := range block {
			if strings.HasPrefix(line, "Namespace:") {
				ns = strings.ToLower(strings.TrimSpace(strings.SplitN(line, ":", 2)[1]))
				break
			}
		}
		if ns != "" && blocked[ns] {
			dropped++
			continue
		}
		kept.WriteString(strings.Join(block, ""))
	}
	if dropped > 0 {
		return kept.String() + nsguard.WithheldNote(dropped, "")
	}
	return kept.String()
}
