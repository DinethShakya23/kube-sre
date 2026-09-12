package main

import (
	"flag"
	"io"
	"strings"
	"testing"
)

func TestFlagsMayFollowPositionals(t *testing.T) {
	for _, args := range [][]string{
		{"decision_log", "ep1", "--through", "4", "-o", "f.json"},
		{"--through", "4", "decision_log", "-o", "f.json", "ep1"},
		{"-o", "f.json", "--through", "4", "decision_log", "ep1"},
	} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		through := fs.Int64("through", -1, "")
		out := fs.String("o", "-", "")
		pos, err := parseMixed(fs, args)
		if err != nil || strings.Join(pos, ",") != "decision_log,ep1" || *through != 4 || *out != "f.json" {
			t.Errorf("%v: %v %v %d %s", args, pos, err, *through, *out)
		}
	}
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if _, err := parseMixed(fs, []string{"a", "--nope"}); err == nil {
		t.Error("an unknown flag after a positional must still be an error")
	}
}
