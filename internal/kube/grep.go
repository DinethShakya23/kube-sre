package kube

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/DinethShakya23/kube-sre/internal/policy"
)

// A grep emulator for `kubectl ... | grep ...`. There is no shell, so this is
// the only pipe. Anything it does not implement is named and refused rather than
// skipped: skipping a value taking flag leaves its value in the pattern
// (`grep -A 3 Traceback` would search for "3 Traceback"), and dropping a combined
// cluster like -iv would run the exact complement of what was asked.

var grepBool = set("-v", "-i", "-E", "-F", "-w", "-x", "-c", "-n", "-o", "-s", "-a")
var grepValue = set("-A", "-B", "-C", "-m", "-e")
var grepLong = map[string]string{
	"--invert-match": "-v", "--ignore-case": "-i", "--extended-regexp": "-E",
	"--fixed-strings": "-F", "--word-regexp": "-w", "--line-regexp": "-x",
	"--count": "-c", "--line-number": "-n", "--only-matching": "-o",
	"--no-messages": "-s", "--text": "-a",
	"--after-context": "-A", "--before-context": "-B", "--context": "-C",
	"--max-count": "-m", "--regexp": "-e",
}

type grepArgs struct {
	opts     map[string]bool
	values   map[string]string
	patterns []string
	operands []string
}

func grepSupported() string {
	var all []string
	for k := range grepBool {
		all = append(all, k)
	}
	for k := range grepValue {
		all = append(all, k)
	}
	sort.Strings(all)
	return strings.Join(all, " ")
}

func unsupportedGrep(segment, flag string) error {
	return fmt.Errorf("grep in pipe segment %q uses %s, which this pipe emulator does not implement. Supported: %s.",
		segment, flag, grepSupported())
}

func takeGrepValue(g *grepArgs, short, value, segment string) error {
	if short == "-e" {
		g.patterns = append(g.patterns, value)
		return nil
	}
	if _, err := strconv.Atoi(strings.TrimLeft(value, "+-")); err != nil || strings.TrimLeft(value, "+-") == "" {
		return fmt.Errorf("grep option %s in pipe segment %q needs a number, got %q.", short, segment, value)
	}
	g.values[short] = value
	return nil
}

func parseGrep(tokens []string, segment string) (*grepArgs, error) {
	g := &grepArgs{opts: map[string]bool{}, values: map[string]string{}}
	endOfFlags := false
	for i := 1; i < len(tokens); i++ {
		tok := tokens[i]
		if endOfFlags || tok == "-" || !strings.HasPrefix(tok, "-") {
			g.operands = append(g.operands, tok)
			continue
		}
		if tok == "--" {
			endOfFlags = true
			continue
		}
		if strings.HasPrefix(tok, "--") {
			name, attached, hasAttached := strings.Cut(tok, "=")
			short, ok := grepLong[name]
			if !ok {
				return nil, unsupportedGrep(segment, name)
			}
			if grepValue[short] {
				if !hasAttached || attached == "" {
					if i+1 >= len(tokens) {
						return nil, fmt.Errorf("grep option %s in %q needs a value.", name, segment)
					}
					i++
					attached = tokens[i]
				}
				if err := takeGrepValue(g, short, attached, segment); err != nil {
					return nil, err
				}
			} else {
				g.opts[short] = true
			}
			continue
		}
		// A short cluster: -iv, -A3, -vA 3
		chars := []rune(tok[1:])
		for j := 0; j < len(chars); j++ {
			short := "-" + string(chars[j])
			if grepValue[short] {
				rest := string(chars[j+1:])
				if rest == "" {
					if i+1 >= len(tokens) {
						return nil, fmt.Errorf("grep option %s in %q needs a value.", short, segment)
					}
					i++
					rest = tokens[i]
				}
				if err := takeGrepValue(g, short, rest, segment); err != nil {
					return nil, err
				}
				break
			}
			if !grepBool[short] {
				return nil, unsupportedGrep(segment, short)
			}
			g.opts[short] = true
		}
	}
	return g, nil
}

// grepRegex builds one regex from every pattern, honouring -F -w -x -i. Several
// bare operands are joined into one pattern on purpose: real grep would treat
// the second as a file, and a pipe segment has no files, so an unquoted multi
// word pattern is the only thing it can have meant.
//
// Go's regexp is RE2, so a pattern that needs backreferences or lookaround is
// refused as invalid where Python's engine would have accepted it.
func grepRegex(g *grepArgs, segment string) (*regexp.Regexp, error) {
	parts := g.patterns
	if len(parts) == 0 && len(g.operands) > 0 {
		parts = []string{strings.Join(g.operands, " ")}
	}
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("grep in pipe segment %q has no pattern.", segment)
	}
	if g.opts["-F"] {
		for i, p := range kept {
			kept[i] = regexp.QuoteMeta(p)
		}
	}
	wrapped := make([]string, len(kept))
	for i, p := range kept {
		wrapped[i] = "(?:" + p + ")"
	}
	body := strings.Join(wrapped, "|")
	switch {
	case g.opts["-x"]:
		body = "^(?:" + body + ")$"
	case g.opts["-w"]:
		body = `\b(?:` + body + `)\b`
	}
	if g.opts["-i"] {
		body = "(?i)" + body
	}
	re, err := regexp.Compile(body)
	if err != nil {
		return nil, fmt.Errorf("grep pattern in %q is not a valid regex: %v", segment, err)
	}
	return re, nil
}

func trimEOL(ln string) string { return strings.TrimRight(ln, "\r\n") }

func runGrep(output string, g *grepArgs, re *regexp.Regexp) string {
	var lines []string
	for _, ln := range policy.SplitLines(output) {
		lines = append(lines, trimEOL(ln))
	}
	invert := g.opts["-v"]
	var hits []int
	for n, ln := range lines {
		if re.MatchString(ln) != invert {
			hits = append(hits, n)
		}
	}
	if m, ok := g.values["-m"]; ok {
		if k, _ := strconv.Atoi(m); k >= 0 && k < len(hits) {
			hits = hits[:k]
		}
	}
	if g.opts["-c"] {
		return fmt.Sprintf("%d\n", len(hits))
	}
	if len(hits) == 0 {
		return "(no matching lines)"
	}
	if g.opts["-o"] && !invert {
		var b strings.Builder
		for _, n := range hits {
			for _, m := range re.FindAllStringSubmatch(lines[n], -1) {
				found := m[0]
				if len(m) > 1 {
					found = ""
					for _, grp := range m[1:] {
						if grp != "" {
							found = grp
							break
						}
					}
				}
				b.WriteString(found + "\n")
			}
		}
		if b.Len() == 0 {
			return "(no matching lines)"
		}
		return b.String()
	}

	atoi := func(key string, def int) int {
		if v, ok := g.values[key]; ok {
			n, _ := strconv.Atoi(v)
			return n
		}
		return def
	}
	ctx := atoi("-C", 0)
	after, before := atoi("-A", ctx), atoi("-B", ctx)
	wanted := map[int]bool{}
	for _, n := range hits {
		for k := maxInt(0, n-before); k < minInt(len(lines), n+after+1); k++ {
			if _, ok := wanted[k]; !ok {
				wanted[k] = false
			}
		}
		wanted[n] = true
	}
	// grep prints the `--` group separator only when context was requested.
	separate := after != 0 || before != 0
	order := make([]int, 0, len(wanted))
	for n := range wanted {
		order = append(order, n)
	}
	sort.Ints(order)
	var out strings.Builder
	prev := -2
	for _, n := range order {
		if separate && prev != -2 && n != prev+1 {
			out.WriteString("--\n")
		}
		if g.opts["-n"] {
			sep := "-"
			if wanted[n] {
				sep = ":"
			}
			fmt.Fprintf(&out, "%d%s%s\n", n+1, sep, lines[n])
		} else {
			out.WriteString(lines[n] + "\n")
		}
		prev = n
	}
	return out.String()
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// applyPipes applies pipe segments such as ["grep foo", "grep -v bar"]. Only
// grep is allowed; any other command, and any grep flag this emulator does not
// implement, is an error so the caller knows to ask differently instead of
// silently getting wrong results.
func applyPipes(output string, segments []string) (string, error) {
	for _, segment := range segments {
		tokens, err := Split(strings.TrimSpace(segment))
		if err != nil {
			return "", err
		}
		if len(tokens) == 0 || tokens[0] != "grep" {
			return "", fmt.Errorf("Pipe segment %q contains disallowed shell characters or unsupported command. "+
				"Only 'grep' is allowed after '|'.", segment)
		}
		g, err := parseGrep(tokens, segment)
		if err != nil {
			return "", err
		}
		re, err := grepRegex(g, segment)
		if err != nil {
			return "", err
		}
		output = runGrep(output, g, re)
	}
	return output, nil
}
