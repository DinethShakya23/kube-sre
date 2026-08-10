package kube

import (
	"strings"
	"testing"
)

const sample = "one info\ntwo Error trace\nthree\nfour info\nfive\nsix ERROR again\n"

func pipe(t *testing.T, out string, seg string) string {
	t.Helper()
	got, err := applyPipes(out, []string{seg})
	if err != nil {
		t.Fatalf("%q: %v", seg, err)
	}
	return got
}

func TestGrepBasics(t *testing.T) {
	if got := pipe(t, sample, "grep info"); got != "one info\nfour info\n" {
		t.Errorf("%q", got)
	}
	if got := pipe(t, sample, "grep -i error"); got != "two Error trace\nsix ERROR again\n" {
		t.Errorf("-i: %q", got)
	}
	if got := pipe(t, sample, "grep -v info"); !strings.Contains(got, "three") || strings.Contains(got, "info") {
		t.Errorf("-v: %q", got)
	}
	if got := pipe(t, sample, "grep -c info"); got != "2\n" {
		t.Errorf("-c: %q", got)
	}
	if got := pipe(t, sample, "grep nomatch"); got != "(no matching lines)" {
		t.Errorf("%q", got)
	}
}

func TestGrepCombinedFlagsAreNotDropped(t *testing.T) {
	// -iv used to run as plain grep, the exact complement of what was asked
	got := pipe(t, sample, "grep -iv error")
	if strings.Contains(strings.ToLower(got), "error") || !strings.Contains(got, "one info") {
		t.Errorf("%q", got)
	}
}

func TestGrepValueFlagsDoNotLeakIntoThePattern(t *testing.T) {
	// `grep -A 3 Traceback` must not search for "3 Traceback"
	log := "a\nTraceback (most recent call last)\n  File x\n  File y\nValueError\nz\n"
	got := pipe(t, log, "grep -A 3 Traceback")
	if !strings.Contains(got, "Traceback") || !strings.Contains(got, "ValueError") || strings.Contains(got, "z") {
		t.Errorf("%q", got)
	}
	if got := pipe(t, log, "grep -A3 Traceback"); !strings.Contains(got, "ValueError") {
		t.Errorf("attached value: %q", got)
	}
	if got := pipe(t, log, "grep --after-context=1 Traceback"); !strings.Contains(got, "File x") || strings.Contains(got, "File y") {
		t.Errorf("long form: %q", got)
	}
}

func TestGrepContextSeparatorsAndNumbers(t *testing.T) {
	in := "a\nHIT\nb\nc\nd\ne\nHIT\nf\n"
	got := pipe(t, in, "grep -C 1 HIT")
	if got != "a\nHIT\nb\n--\ne\nHIT\nf\n" {
		t.Errorf("%q", got)
	}
	if got := pipe(t, in, "grep -n HIT"); got != "2:HIT\n7:HIT\n" {
		t.Errorf("-n: %q", got)
	}
	if got := pipe(t, in, "grep -n -A 1 HIT"); !strings.HasPrefix(got, "2:HIT\n3-b\n") {
		t.Errorf("context lines use a dash: %q", got)
	}
	if got := pipe(t, in, "grep -m 1 HIT"); got != "HIT\n" {
		t.Errorf("-m: %q", got)
	}
}

func TestGrepOnlyMatchingFixedWordLine(t *testing.T) {
	if got := pipe(t, "abc123 def45\n", "grep -o [0-9]+"); got != "123\n45\n" {
		t.Errorf("-o: %q", got)
	}
	if got := pipe(t, "a.b\naxb\n", "grep -F a.b"); got != "a.b\n" {
		t.Errorf("-F: %q", got)
	}
	if got := pipe(t, "cat\nconcat\n", "grep -w cat"); got != "cat\n" {
		t.Errorf("-w: %q", got)
	}
	if got := pipe(t, "cat\ncat food\n", "grep -x cat"); got != "cat\n" {
		t.Errorf("-x: %q", got)
	}
	if got := pipe(t, "foo\nbar\nbaz\n", "grep -e foo -e baz"); got != "foo\nbaz\n" {
		t.Errorf("-e: %q", got)
	}
	if got := pipe(t, "a b\nab\n", "grep a b"); got != "a b\n" {
		t.Errorf("bare operands join into one pattern: %q", got)
	}
}

func TestGrepRefusesWhatItDoesNotImplement(t *testing.T) {
	for _, seg := range []string{"grep -P x", "grep -r x", "grep --color x", "grep -A x y", "grep -A", "grep", "grep --foo=bar x"} {
		if _, err := applyPipes("x", []string{seg}); err == nil {
			t.Errorf("%q must be refused, not silently skipped", seg)
		}
	}
	if _, err := applyPipes("x", []string{"awk '{print}'"}); err == nil || !strings.Contains(err.Error(), "Only 'grep'") {
		t.Errorf("non grep: %v", err)
	}
	if _, err := applyPipes("x", []string{"grep (unclosed"}); err == nil || !strings.Contains(err.Error(), "not a valid regex") {
		t.Errorf("bad regex: %v", err)
	}
}

func TestGrepChains(t *testing.T) {
	got, err := applyPipes(sample, []string{"grep info", "grep -v one"})
	if err != nil || got != "four info\n" {
		t.Errorf("%q %v", got, err)
	}
}
