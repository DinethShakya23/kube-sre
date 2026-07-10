package kube

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const MaxOutput = 8000

type Runner struct {
	Bin        string
	Kubeconfig string
	Timeout    time.Duration
}

func NewRunner(kubeconfig string) *Runner {
	return &Runner{Bin: "kubectl", Kubeconfig: kubeconfig, Timeout: 30 * time.Second}
}

// Split breaks a command line into words, honoring quotes.
func Split(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	var quote rune
	started := false
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			started = true
		case r == ' ' || r == '\t' || r == '\n':
			if started || cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteRune(r)
		}
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote")
	}
	if started || cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out, nil
}

// splitPipes separates "kubectl args | grep x | grep -v y" into segments.
func splitPipes(s string) []string {
	var parts []string
	var cur strings.Builder
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
			cur.WriteRune(r)
		case r == '|':
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	return append(parts, cur.String())
}

type grepOpts struct {
	invert, ignoreCase, count, extended bool
	pattern                             string
}

func parseGrep(tokens []string) (grepOpts, error) {
	var g grepOpts
	for _, t := range tokens[1:] {
		if strings.HasPrefix(t, "-") && len(t) > 1 && g.pattern == "" {
			for _, f := range t[1:] {
				switch f {
				case 'v':
					g.invert = true
				case 'i':
					g.ignoreCase = true
				case 'c':
					g.count = true
				case 'E':
					g.extended = true
				default:
					return g, fmt.Errorf("grep flag -%c is not supported", f)
				}
			}
			continue
		}
		if g.pattern != "" {
			return g, errors.New("grep takes one pattern")
		}
		g.pattern = t
	}
	if g.pattern == "" {
		return g, errors.New("grep needs a pattern")
	}
	return g, nil
}

func runGrep(in string, g grepOpts) (string, error) {
	pat := g.pattern
	if !g.extended {
		pat = regexp.QuoteMeta(pat)
	}
	if g.ignoreCase {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return "", fmt.Errorf("bad grep pattern: %w", err)
	}
	var kept []string
	for _, line := range strings.Split(strings.TrimRight(in, "\n"), "\n") {
		if re.MatchString(line) != g.invert {
			kept = append(kept, line)
		}
	}
	if g.count {
		return fmt.Sprintf("%d\n", len(kept)), nil
	}
	if len(kept) == 0 {
		return "", nil
	}
	return strings.Join(kept, "\n") + "\n", nil
}

func applyPipes(out string, segments []string) (string, error) {
	for _, seg := range segments {
		tokens, err := Split(strings.TrimSpace(seg))
		if err != nil {
			return "", err
		}
		if len(tokens) == 0 || tokens[0] != "grep" {
			return "", fmt.Errorf("only grep is allowed after |, got %q", seg)
		}
		g, err := parseGrep(tokens)
		if err != nil {
			return "", err
		}
		if out, err = runGrep(out, g); err != nil {
			return "", err
		}
	}
	return out, nil
}

func capOutput(s string) string {
	if len(s) <= MaxOutput {
		return s
	}
	return s[:MaxOutput] + "\n[output truncated]"
}

// Args parses a command line into checked kubectl args plus any grep pipes.
func Args(cmdline string) (args []string, pipes []string, err error) {
	segs := splitPipes(cmdline)
	args, err = Split(segs[0])
	if err != nil {
		return nil, nil, err
	}
	if len(args) > 0 && args[0] == "kubectl" {
		args = args[1:]
	}
	if err = Check(args); err != nil {
		return nil, nil, err
	}
	return args, segs[1:], nil
}

// Exec runs already-approved args and applies the pipes.
func (r *Runner) Exec(ctx context.Context, args, pipes []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()

	full := args
	if r.Kubeconfig != "" {
		full = append([]string{"--kubeconfig", r.Kubeconfig}, args...)
	}
	cmd := exec.CommandContext(ctx, r.Bin, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	out, err := applyPipes(stdout.String(), pipes)
	if err != nil {
		return "", err
	}
	return capOutput(out), nil
}
