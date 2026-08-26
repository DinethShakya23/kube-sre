package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestLevelNames(t *testing.T) {
	cases := map[string]slog.Level{"debug": slog.LevelDebug, "INFO": slog.LevelInfo, "warning": slog.LevelWarn, "WARN": slog.LevelWarn,
		"error": slog.LevelError, "critical": slog.LevelError, "": slog.LevelInfo, "loud": slog.LevelInfo}
	for in, want := range cases {
		if got := Level(in); got != want {
			t.Errorf("%q: %v want %v", in, got, want)
		}
	}
}

func TestFormatsAndFiltering(t *testing.T) {
	var b bytes.Buffer
	l := New(&b, "WARNING", "json")
	l.Info("quiet")
	l.Warn("loud", "session", "s1")
	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(b.Bytes()), &line); err != nil {
		t.Fatalf("%v: %s", err, b.String())
	}
	if line["msg"] != "loud" || line["session"] != "s1" || line["level"] != "WARN" {
		t.Errorf("%v", line)
	}
	b.Reset()
	New(&b, "info", "text").Info("hello", "k", "v")
	if !strings.Contains(b.String(), `msg=hello`) || !strings.Contains(b.String(), "k=v") {
		t.Errorf("%s", b.String())
	}
}
