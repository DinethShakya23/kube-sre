package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBoundBytesCutsOnALineAndSaysSo(t *testing.T) {
	text := strings.Repeat("a line of text\n", 100)
	got := boundBytes(text, 300)
	if len(got) > 300 || !strings.HasSuffix(got, "budget]") || !strings.Contains(got, "truncated") {
		t.Errorf("%d %q", len(got), got[len(got)-60:])
	}
	if body := strings.TrimSuffix(got, truncMarker); strings.HasSuffix(body, "a li") || !strings.HasSuffix(body, "text") {
		t.Errorf("must cut on a line boundary: %q", body[len(body)-20:])
	}
	if boundBytes("short", 300) != "short" {
		t.Error("under the budget is untouched")
	}
	multi := strings.Repeat("héllo wörld ✓\n", 200)
	if out := boundBytes(multi, 500); !utf8.ValidString(out) || len(out) > 500 {
		t.Errorf("valid UTF-8 and within budget: %d", len(out))
	}
}

func TestRenderers(t *testing.T) {
	eps := []FileEpisode{
		{Namespace: "shop", Outcome: "resolved", Summary: "web crashed\nagain", RootCause: "bad image", Verified: true},
		{Namespace: "api", Summary: "a report", RootCause: "x"},
		{Namespace: "shop", Outcome: "partial", Summary: "s"},
	}
	c := RenderClusterMD("prod-1", eps, 25000)
	for _, want := range []string{"# CLUSTER.md — prod-1", "## Namespaces seen\n- api\n- shop\n", "- [resolved] web crashed again", "- [report_only] a report", projectionNote} {
		if !strings.Contains(c, want) {
			t.Errorf("missing %q in\n%s", want, c)
		}
	}
	if strings.Count(c, "- shop") != 1 {
		t.Error("namespaces are listed once")
	}
	m := RenderMemoryMD("prod-1", "## Memory themes (this cluster)\n- Theme 'x'", eps, 25000)
	if !strings.Contains(m, "- **bad image** — web crashed again") || strings.Contains(m, "**x**") || !strings.Contains(m, "## Memory themes") {
		t.Errorf("only verified resolutions with a root cause: %s", m)
	}
	if !strings.Contains(RenderClusterMD("c", nil, 1000), "_none recorded yet_") || !strings.Contains(RenderMemoryMD("c", "", nil, 1000), "_no verified resolutions yet_") {
		t.Error("empty states")
	}
}

func TestRegenerateWritesBothFilesAndCountsBytes(t *testing.T) {
	s := roll(t, map[string]string{"MEMORY_SUMMARY_TREE": "true"})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		verifiedEp(t, s, "OOMKilled", "limit too low", float64(3-i))
	}
	s.BuildSummaryTree(ctx)
	dir := filepath.Join(t.TempDir(), "nested", "ki-memory")
	n := s.RegenerateFilePlane(ctx, "c1", dir, 25000)
	cm, err1 := os.ReadFile(filepath.Join(dir, "CLUSTER.md"))
	mm, err2 := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
	if err1 != nil || err2 != nil || n != len(cm)+len(mm) || n == 0 {
		t.Fatalf("%d %v %v", n, err1, err2)
	}
	if !strings.Contains(string(mm), "limit too low") || !strings.Contains(string(mm), "Memory themes") {
		t.Errorf("%s", mm)
	}
}

func TestRegenerateFailureIsRegisteredNotRaised(t *testing.T) {
	s := roll(t, nil)
	blocker := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(blocker, []byte("x"), 0o600)
	if n := s.RegenerateFilePlane(context.Background(), "c1", filepath.Join(blocker, "sub"), 1000); n != 0 {
		t.Errorf("%d", n)
	}
	if f := s.Live.DrainPassFailures(); len(f) != 1 || f[0][0] != "file_plane_bytes" {
		t.Errorf("%v", f)
	}
}
