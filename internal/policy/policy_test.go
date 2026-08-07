package policy

import (
	"strings"
	"testing"
)

func TestSplitKeepsBodyByteForByte(t *testing.T) {
	in := "NAME  READY\r\nweb   1/1\n[Protected] 2 namespace(s) withheld.\nlast\n[truncated: 5 chars omitted]"
	body, pol := Split(in)
	if body != "NAME  READY\r\nweb   1/1\nlast\n" {
		t.Errorf("body %q", body)
	}
	if !strings.Contains(pol, "[Protected]") || !strings.Contains(pol, "[truncated") {
		t.Errorf("policy %q", pol)
	}
	same := "plain\nlines\n"
	if b, p := Split(same); b != same || p != "" {
		t.Errorf("no policy line must come back unchanged: %q %q", b, p)
	}
}

func TestSplitLinesEndings(t *testing.T) {
	got := SplitLines("a\nb\r\nc\rd")
	want := []string{"a\n", "b\r\n", "c\r", "d"}
	if strings.Join(got, "|") != strings.Join(want, "|") || len(got) != 4 {
		t.Errorf("%q", got)
	}
	if len(SplitLines("")) != 0 {
		t.Error("empty")
	}
}

func TestMarkersAreCoveredByThePatterns(t *testing.T) {
	m := TruncationMarker(12, "", "")
	for _, p := range MarkerPatterns {
		if !strings.Contains(m, p) {
			t.Errorf("default marker %q must contain %q", m, p)
		}
	}
	if !strings.Contains(TruncationMarker(3, "rows", "use -n"), "3 rows omitted - use -n") {
		t.Error("unit and hint")
	}
	if !LineRE.MatchString(m) {
		t.Error("a truncation marker must count as a policy line")
	}
}

func TestUnavailableIsATrailingLine(t *testing.T) {
	out := MarkUnavailable("[Error] kubectl is not installed.", "kubectl is not on PATH.")
	if !strings.HasPrefix(out, "[Error]") {
		t.Errorf("readers key on how the reply starts: %q", out)
	}
	if !strings.Contains(out, UnavailableMarker) || !strings.HasSuffix(out, "will not change that.") {
		t.Errorf("%q", out)
	}
	if !LineRE.MatchString(UnavailableNotice("x")) {
		t.Error("must be a policy line")
	}
}
