package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func loadWith(env map[string]string) *Config { return Load(func(k string) string { return env[k] }) }

func TestActiveFlagsAreOnBooleansOnly(t *testing.T) {
	c := loadWith(map[string]string{"CORTEX_V5_ENABLED": "true", "KI_V5_CHANGE_LEDGER": "true", "KI_V5_HEARTBEAT_SECONDS": "5", "LOG_LEVEL": "debug"})
	got := strings.Join(c.ActiveFlags(), ",")
	if !strings.Contains(got, "CORTEX_V5_ENABLED") || !strings.Contains(got, "KI_V5_CHANGE_LEDGER") || strings.Contains(got, "HEARTBEAT") || strings.Contains(got, "LOG_LEVEL") {
		t.Errorf("knobs and non experimental settings are not identity: %s", got)
	}
}

func TestUnwiredFlagsAreReportedNotCalledActive(t *testing.T) {
	old := UnwiredExperimental
	defer func() { UnwiredExperimental = old }()
	UnwiredExperimental = map[string]bool{"KI_V5_RIGHTSIZING": true, "KI_V5_SPEND_OUT_PRICE_PER_1K": true, "KI_V5_STAGE_SIZE": true}

	c := loadWith(map[string]string{"KI_V5_RIGHTSIZING": "true", "KI_V5_SPEND_OUT_PRICE_PER_1K": "0.99", "KI_V5_STAGE_SIZE": "1", "CORTEX_V5_ENABLED": "true"})
	for _, f := range c.ActiveFlags() {
		if f == "KI_V5_RIGHTSIZING" {
			t.Error("an unwired switch must not be reported active")
		}
	}
	got := strings.Join(c.SetButUnwired(), ",")
	if got != "KI_V5_RIGHTSIZING,KI_V5_SPEND_OUT_PRICE_PER_1K" {
		t.Errorf("a moved knob is reported, a knob at its default is not: %s", got)
	}
	if line := c.VersionLine("v1", "0.1.0"); !strings.Contains(line, "set but NOT WIRED, no effect: KI_V5_RIGHTSIZING") || !strings.Contains(line, "CORTEX_V5_ENABLED") {
		t.Errorf("%s", line)
	}
	// A knob set to zero is a deliberate setting, not an absent one.
	UnwiredExperimental = map[string]bool{"KI_V5_AGENT_COST_RATE_CAP": true}
	if got := loadWith(map[string]string{"KI_V5_AGENT_COST_RATE_CAP": "0"}).SetButUnwired(); len(got) != 1 {
		t.Errorf("%v", got)
	}
}

func TestDegradedFlagsNeedTheHierarchy(t *testing.T) {
	c := loadWith(map[string]string{"MEMORY_PROMOTION": "true", "KI_V5_STATISTICAL_PROMOTION": "true", "KI_V5_CHANGE_LEDGER": "true"})
	if got := c.DegradedFlags("ready"); len(got) != 0 {
		t.Errorf("%v", got)
	}
	got := strings.Join(c.DegradedFlags("degraded"), ",")
	if !strings.Contains(got, "MEMORY_PROMOTION") || !strings.Contains(got, "KI_V5_STATISTICAL_PROMOTION") || strings.Contains(got, "CHANGE_LEDGER") {
		t.Errorf("only slices that run inside the hierarchy: %s", got)
	}
}

// TestFlagWiring is the production check on UnwiredExperimental: it reads the source,
// so wiring a flag and forgetting to delete it here fails, and so does listing a flag
// that is read.
func TestFlagWiring(t *testing.T) {
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatal(err)
	}
	fieldOf := map[string]string{}
	re := regexp.MustCompile(`(\w+):\s+(?:boolean|str|num|numMin|float|anyInt)\("([A-Z0-9_]+)"`)
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		if isExperimental(m[2]) {
			fieldOf[m[2]] = m[1]
		}
	}
	if len(fieldOf) < 50 {
		t.Fatalf("expected the experimental settings to be found, got %d", len(fieldOf))
	}

	var code strings.Builder
	for _, root := range []string{"../../internal", "../../cmd"} {
		_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") || strings.Contains(p, "internal/config/") {
				return nil
			}
			b, _ := os.ReadFile(p)
			code.Write(b)
			return nil
		})
	}
	body := code.String()
	var unread []string
	for env, field := range fieldOf {
		if !regexp.MustCompile(`\.` + field + `\b`).MatchString(body) {
			unread = append(unread, env)
		}
	}
	sort.Strings(unread)
	var listed []string
	for k := range UnwiredExperimental {
		listed = append(listed, k)
	}
	sort.Strings(listed)
	if fmt.Sprint(unread) != fmt.Sprint(listed) {
		t.Errorf("UnwiredExperimental must be exactly the settings no code reads.\n read by nothing: %v\n listed:          %v", unread, listed)
	}
}
