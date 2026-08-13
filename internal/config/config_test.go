package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaults(t *testing.T) {
	c := Load(env(nil))
	if c.LLMProvider != "openai" || c.CoordModel != "gpt-4o" || c.SubModel != "gpt-4o-mini" {
		t.Errorf("llm defaults wrong: %+v", c)
	}
	if !c.OpenAccess() {
		t.Error("no keys means open access")
	}
	if c.KubectlTimeout.Seconds() != 30 || c.KubectlWriteTimeout.Seconds() != 300 {
		t.Error("timeout defaults wrong")
	}
	if !c.BlockedNamespaces["kube-system"] || !c.BlockedResources["secret"] {
		t.Error("default blocks missing")
	}
	if c.GraphRecursionLimit != 120 || c.CoordinatorRecursions != 150 {
		t.Error("recursion defaults wrong")
	}
}

func TestBlockedFoldsCaseAndKeepsCredentials(t *testing.T) {
	c := Load(env(map[string]string{
		"KUBECTL_BLOCKED_NAMESPACES": " Kube-System , prod ",
		"KUBECTL_BLOCKED_RESOURCES":  "ConfigMap,ingress",
	}))
	if !c.BlockedNamespaces["kube-system"] || !c.BlockedNamespaces["prod"] {
		t.Errorf("namespaces: %v", c.BlockedNamespaces)
	}
	// configuring the list replaces it, but the credential types cannot be dropped
	for _, r := range []string{"configmap", "ingress", "secret", "secrets", "serviceaccount", "serviceaccounts"} {
		if !c.BlockedResources[r] {
			t.Errorf("resource %q should be blocked", r)
		}
	}
}

func TestAuthKeys(t *testing.T) {
	c := Load(env(map[string]string{"KUBESRE_ADMIN_KEYS": "a, b"}))
	if c.OpenAccess() || len(c.AdminKeys) != 2 || c.AdminKeys[1] != "b" {
		t.Errorf("keys: %v", c.AdminKeys)
	}
}

func TestWarnings(t *testing.T) {
	c := Load(env(map[string]string{
		"KUBECTL_BLOCKED_NAMESPACES": "kube-*",
		"ALLOWED_ORIGINS":            "*,example.com,http://a.example/",
	}))
	w := strings.Join(c.Warnings(), "\n")
	for _, want := range []string{"kube-*", "any site", "no scheme", "trailing slash"} {
		if !strings.Contains(w, want) {
			t.Errorf("missing warning about %q in:\n%s", want, w)
		}
	}
}

func TestBadNumbersFallBackAndAreReported(t *testing.T) {
	c := Load(env(map[string]string{"KUBECTL_TIMEOUT_SECONDS": "abc", "AGENT_GRAPH_RECURSION_LIMIT": "-4", "LLM_TEMPERATURE": "hot"}))
	if c.KubectlTimeout.Seconds() != 30 || c.GraphRecursionLimit != 120 || c.LLMTemperature != 0 {
		t.Error("bad numbers should fall back to defaults")
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("a number that could not be read must fail validation")
	}
	for _, want := range []string{"KUBECTL_TIMEOUT_SECONDS", "AGENT_GRAPH_RECURSION_LIMIT", "LLM_TEMPERATURE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %s: %v", want, err)
		}
	}
	if err := Load(env(nil)).Validate(); err != nil {
		t.Errorf("defaults are valid: %v", err)
	}
}

func TestValidateRefusesUnknownProviderAndMode(t *testing.T) {
	c := Load(env(map[string]string{"LLM_PROVIDER": "bard", "SNAPSHOT_SUFFICIENCY_MODE": "loud"}))
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "LLM_PROVIDER") || !strings.Contains(err.Error(), "SNAPSHOT_SUFFICIENCY_MODE") {
		t.Errorf("%v", err)
	}
	if Load(env(map[string]string{"LLM_PROVIDER": "Azure"})).Validate() != nil {
		t.Error("the provider is case insensitive")
	}
}

func TestQwenAliasTargetsDashScope(t *testing.T) {
	c := Load(env(map[string]string{"LLM_PROVIDER": "qwen", "DASHSCOPE_API_KEY": "k"}))
	if c.OpenAIKey != "k" || !strings.Contains(c.OpenAIBase, "dashscope") || c.CoordModel != "qwen-max" || c.SubModel != "qwen-plus" {
		t.Errorf("%+v", c)
	}
	c = Load(env(map[string]string{"LLM_PROVIDER": "qwen", "QWEN_API_KEY": "q", "OPENAI_COORDINATOR_MODEL": "qwen-turbo", "OPENAI_BASE_URL": "http://x"}))
	if c.OpenAIKey != "q" || c.OpenAIBase != "http://x" || c.CoordModel != "qwen-turbo" {
		t.Errorf("explicit settings win over the alias: %+v", c)
	}
}

func TestLLMWarnings(t *testing.T) {
	if w := Load(env(nil)).LLMWarnings(); len(w) != 1 || !strings.Contains(w[0], "OPENAI_API_KEY") {
		t.Errorf("%v", w)
	}
	if w := Load(env(map[string]string{"LLM_PROVIDER": "azure"})).LLMWarnings(); len(w) != 1 || !strings.Contains(w[0], "AZURE_OPENAI_ENDPOINT") {
		t.Errorf("%v", w)
	}
	if w := Load(env(map[string]string{"OPENAI_API_KEY": "k"})).LLMWarnings(); len(w) != 0 {
		t.Errorf("%v", w)
	}
}

func TestDotenvFilesAndPrecedence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, ".kube-sre"), 0o755); err != nil {
		t.Fatal(err)
	}
	home := "# comment\nexport LLM_PROVIDER=azure\nOPENAI_API_KEY=\"from-home\"\nLOG_LEVEL=debug # trailing\n"
	if err := os.WriteFile(filepath.Join(dir, ".kube-sre", ".env"), []byte(home), 0o600); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	old, _ := os.Getwd()
	defer os.Chdir(old)
	os.Chdir(cwd)
	os.WriteFile(".env", []byte("llm_provider=openai\n"), 0o600)
	t.Setenv("LOG_LEVEL", "warning")
	c := FromEnvironment()
	if c.LLMProvider != "openai" {
		t.Errorf("the project .env beats the home file: %s", c.LLMProvider)
	}
	if c.OpenAIKey != "from-home" {
		t.Errorf("quotes are stripped: %q", c.OpenAIKey)
	}
	if c.LogLevel != "WARNING" {
		t.Errorf("real environment variables win over both files: %s", c.LogLevel)
	}
}

func TestReflexionDefaults(t *testing.T) {
	c := Load(env(nil))
	if !c.Reflexion || c.ReflexionMinConf != 0.7 || !c.ReflexionVerify || c.ReflexionCooldownHrs != 1 || c.ReflexionDecayDays != 30 {
		t.Errorf("%+v", c)
	}
}

func TestDatabaseChoice(t *testing.T) {
	if Load(env(nil)).Postgres() {
		t.Error("nothing set means sqlite")
	}
	c := Load(env(map[string]string{"POSTGRES_HOST": "db", "POSTGRES_PASSWORD": "p@ss word"}))
	if !c.Postgres() {
		t.Error("host set means postgres")
	}
	if got := c.DSN(); got != "postgres://kube-sre:p%40ss%20word@db:5432/kube-sre" {
		t.Errorf("dsn=%s", got)
	}
	c = Load(env(map[string]string{"POSTGRES_HOST": "db", "USE_SQLITE": "true"}))
	if c.Postgres() {
		t.Error("USE_SQLITE wins")
	}
	c = Load(env(map[string]string{"DATABASE_URL": "postgres://x/y"}))
	if !c.Postgres() || c.DSN() != "postgres://x/y" {
		t.Error("database url should win")
	}
}

func TestSensoriumSettings(t *testing.T) {
	c := Load(env(nil))
	if !c.Sensorium || c.SensoriumQueueSize != 10000 || len(c.SensoriumNamespaces) != 0 {
		t.Errorf("defaults: %+v", c)
	}
	c = Load(env(map[string]string{"SENSORIUM_WATCH_NAMESPACES": " a, b ,", "SENSORIUM_ENABLED": "false"}))
	if c.Sensorium || len(c.SensoriumNamespaces) != 2 || c.SensoriumNamespaces[1] != "b" {
		t.Errorf("set: %+v", c)
	}
}
