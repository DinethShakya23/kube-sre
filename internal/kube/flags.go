package kube

import "strings"

func set(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, it := range items {
		m[it] = true
	}
	return m
}

// kubectl global flags that take the next token as their value. Needed so a
// flag's value is never mistaken for the verb: in `kubectl -n prod delete deploy
// api` the token after -n is `prod`, and neither is the verb.
var valueFlags = set(
	"-n", "--namespace", "-o", "--output", "--context", "--cluster", "--user", "--kubeconfig",
	"--server", "-s", "--token", "--as", "--as-group", "--as-uid", "--request-timeout",
	"--cache-dir", "-l", "--selector", "--field-selector", "--certificate-authority",
	"--client-certificate", "--client-key", "--tls-server-name", "--username", "--password",
	"-f", "--filename", "--log-flush-frequency", "--v", "--profile", "--profile-output",
	"--chunk-size",
)

// kubectl's boolean global flags. pflag accepts a boolean as a bare token, so a
// boolean listed in valueFlags would make the walk swallow the next token, which
// is the verb, and every gate reads the verb from that one walk. The two sets
// must stay disjoint; a test asserts it.
var booleanGlobalFlags = set(
	"--warnings-as-errors", "--insecure-skip-tls-verify",
	"--disable-compression", "--match-server-version",
)

// Flags that decide which cluster the command talks to and as whom. Which
// cluster and which identity are deployment configuration, not arguments, so a
// caller that can override them has stepped outside every guarantee here. They
// are refused rather than stripped: dropping a flag would answer a different
// question than the one asked.
var connectionFlags = set(
	"--as", "--as-group", "--as-uid",
	"--user", "--username", "--password", "--token",
	"--server", "-s", "--kubeconfig", "--context", "--cluster",
	"--insecure-skip-tls-verify", "--certificate-authority",
	"--client-certificate", "--client-key", "--tls-server-name",
)

// A flag's name, for `--flag=value` and `--flag value` alike.
func flagName(tok string) string {
	if i := strings.Index(tok, "="); i >= 0 {
		return tok[:i]
	}
	return tok
}

// ConnectionFlagIn returns the first connection or identity flag, or "". The
// --as family is also matched by prefix, since every impersonation flag is
// spelled --as*, so a future one is refused without this list changing.
func ConnectionFlagIn(tokens []string) string {
	for _, tok := range tokens {
		if !strings.HasPrefix(tok, "-") {
			continue
		}
		name := flagName(tok)
		if connectionFlags[name] || strings.HasPrefix(name, "--as") {
			return name
		}
	}
	return ""
}

// identityIsAuthorised is true iff every connection or identity token in the
// command is exactly the one the application placed there.
func identityIsAuthorised(tokens []string, authorised string) bool {
	var found []string
	for _, tok := range tokens {
		if !strings.HasPrefix(tok, "-") {
			continue
		}
		name := flagName(tok)
		if connectionFlags[name] || strings.HasPrefix(name, "--as") {
			found = append(found, tok)
		}
	}
	return len(found) == 1 && found[0] == authorised
}

// skipFlags returns the index of the first token from start that is neither a
// flag nor a flag's value.
func skipFlags(tokens []string, start int) int {
	i := start
	for i < len(tokens) {
		tok := tokens[i]
		if !strings.HasPrefix(tok, "-") {
			return i
		}
		// `--flag=value` carries its value inline; `--flag value` consumes the next.
		if !strings.Contains(tok, "=") && valueFlags[tok] {
			i += 2
		} else {
			i++
		}
	}
	return len(tokens)
}

// operandIndex is the index of the verb's operand, or len(tokens) when there is
// none. Skip the global flags to reach the verb, then the verb's own flags to
// reach what it acts on.
func operandIndex(tokens []string) int {
	return skipFlags(tokens, skipFlags(tokens, 1)+1)
}

// operandAfterVerb is the first token after the verb that is neither a flag nor
// a flag's value, lowercased: the resource kind (`delete namespace shop`) or the
// subcommand (`set image ...`).
func operandAfterVerb(tokens []string) string {
	i := operandIndex(tokens)
	if i < len(tokens) {
		return strings.ToLower(tokens[i])
	}
	return ""
}

// ExtractVerb returns the kubectl verb, ignoring global flags that precede it.
// `kubectl -n prod delete deployment api` is valid kubectl and puts -n where a
// fixed index would look for the verb.
func ExtractVerb(tokens []string) string {
	i := skipFlags(tokens, 1)
	if i < len(tokens) {
		return tokens[i]
	}
	return ""
}

// Shorthands that never take a value, so a single dash group continues past
// them, measured from `kubectl <verb> --help` rather than recalled.
const booleanShorthands = "AhiqRtw"

// -f and -p are the only letters that are boolean in one subcommand and a value
// flag in every other (--follow and --previous on `logs`). The verb decides.
var verbBooleanShorthands = map[string]string{"logs": "fp"}

// FlagValue reads a flag in every form pflag accepts: -o json, -o=json, -ojson,
// --output json, --output=json, and a combined shorthand group such as -Rn x.
// A group is walked left to right the way pflag walks it: the first letter that
// takes a value swallows the rest, so scanning the whole group for the target
// letter would be wrong (-ojson contains an n). The walk stops at any letter not
// known to be boolean. An absent or empty value returns "".
func FlagValue(args []string, short, long, verb string) string {
	letter := strings.TrimPrefix(short, "-")
	booleans := booleanShorthands + verbBooleanShorthands[verb]
	for i, arg := range args {
		if arg == short || arg == long {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		if strings.HasPrefix(arg, long+"=") {
			return arg[len(long)+1:]
		}
		// `--no-headers` starts with a dash and the same letter but is another
		// flag; `-` alone is stdin, not a group.
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && len(arg) > 1 {
			group := arg[1:]
			for j, ch := range group {
				if string(ch) == letter {
					rest := group[j+len(string(ch)):]
					if strings.HasPrefix(rest, "=") {
						return rest[1:]
					}
					if rest != "" {
						return rest
					}
					if i+1 < len(args) {
						return args[i+1]
					}
					return ""
				}
				if !strings.ContainsRune(booleans, ch) {
					break
				}
			}
		}
	}
	return ""
}

func extractNamespace(args []string, verb string) string {
	return FlagValue(args, "-n", "--namespace", verb)
}

// isAllNamespaces: -A is boolean, so kubectl accepts the bare flag and the
// explicit =true and =false forms. An explicit false is not a cluster wide request.
func isAllNamespaces(args []string) bool {
	for _, arg := range args {
		if arg == "-A" || arg == "--all-namespaces" {
			return true
		}
		if strings.HasPrefix(arg, "--all-namespaces=") {
			v := strings.ToLower(strings.TrimSpace(arg[len("--all-namespaces="):]))
			return v != "false" && v != "0" && v != "no"
		}
	}
	return false
}

var resourceVerbs = set("get", "describe", "delete", "edit", "patch", "apply", "create", "replace", "label", "annotate")

// extractResourceType returns the resource type named by the command, or "" for
// verbs whose operand is not a resource type (logs, exec, rollout).
func extractResourceType(verb string, args []string) string {
	if !resourceVerbs[verb] {
		return ""
	}
	// `shorthand/name` names the type before the slash.
	return strings.SplitN(operandAfterVerb(args), "/", 2)[0]
}

var namespaceKinds = set("namespace", "namespaces", "ns")

// targetedNamespaces lists every namespace named as the command's target, as in
// `kubectl delete ns kube-system`, where the name is positional rather than -n.
// It returns a list: `ns/kube-system` carries the name inside the operand, and
// `delete ns shop kube-system` names as many as it likes, so a guard that
// reports one answer misses the second. Empty for a bare `get namespaces`.
func targetedNamespaces(verb string, args []string) []string {
	if !namespaceKinds[extractResourceType(verb, args)] {
		return nil
	}
	at := operandIndex(args)
	if at >= len(args) {
		return nil
	}
	var out []string
	if parts := strings.SplitN(args[at], "/", 2); len(parts) == 2 && parts[1] != "" {
		out = append(out, strings.ToLower(parts[1]))
	}
	for i := skipFlags(args, at+1); i < len(args); i = skipFlags(args, i+1) {
		seg := strings.Split(args[i], "/")
		out = append(out, strings.ToLower(seg[len(seg)-1]))
	}
	return out
}

// kubectl's short names for the resource types the blocklist cares about. Only
// credential relevant aliases are listed: this is a security floor, not a mirror
// of `kubectl api-resources`.
var resourceAliases = map[string]string{
	"sa": "serviceaccounts", "secret": "secrets", "serviceaccount": "serviceaccounts",
}

var esSuffixes = []string{"s", "x", "z", "ch", "sh"}

func hasAnySuffix(w string, suffixes []string) bool {
	for _, s := range suffixes {
		if strings.HasSuffix(w, s) {
			return true
		}
	}
	return false
}

// numberForms returns {singular, plural} for a resource name. Naive +"s" gets
// configmaps right and ingresses wrong. Over-generation is harmless: an extra
// string that names no resource never matches anything kubectl accepts.
func numberForms(word string) map[string]bool {
	out := map[string]bool{}
	if word == "" {
		return out
	}
	out[word] = true
	switch {
	case hasAnySuffix(word, esSuffixes):
		out[word+"es"] = true
	case strings.HasSuffix(word, "y") && len(word) > 1 && !strings.ContainsRune("aeiou", rune(word[len(word)-2])):
		out[word[:len(word)-1]+"ies"] = true
	default:
		out[word+"s"] = true
	}
	switch {
	case strings.HasSuffix(word, "ies") && len(word) > 3:
		out[word[:len(word)-3]+"y"] = true
	case strings.HasSuffix(word, "es") && hasAnySuffix(word[:len(word)-2], esSuffixes):
		out[word[:len(word)-2]] = true
	case strings.HasSuffix(word, "s") && len(word) > 1:
		out[word[:len(word)-1]] = true
	}
	return out
}

// resourceSpellings is every spelling of a resource that must be tested against
// the blocklist: the raw token, the name before `.version.group`
// (`secrets.v1.`), the canonical name for a short alias, and singular and plural
// of each, since `get configmap` and `get configmaps` are one command.
func resourceSpellings(resource string) map[string]bool {
	out := map[string]bool{}
	if resource == "" {
		return out
	}
	raw := strings.ToLower(resource)
	base := strings.SplitN(raw, ".", 2)[0]
	out[raw], out[base] = true, true
	if a, ok := resourceAliases[base]; ok {
		out[a] = true
	} else {
		out[base] = true
	}
	for form := range out {
		for f := range numberForms(form) {
			out[f] = true
		}
	}
	delete(out, "")
	return out
}

func intersects(a, b map[string]bool) bool {
	for k := range a {
		if b[k] {
			return true
		}
	}
	return false
}
