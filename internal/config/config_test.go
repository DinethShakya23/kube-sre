package config

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaults(t *testing.T) {
	c := Load(env(nil))
	if c.LLMProvider != "azure" || c.CoordModel != "gpt-4o" || c.SubModel != "gpt-4o-mini" {
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
	for _, r := range []string{"configmap", "configmaps", "ingress", "ingresses", "secret", "serviceaccounts"} {
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

func TestBadNumbersFallBack(t *testing.T) {
	c := Load(env(map[string]string{"KUBECTL_TIMEOUT_SECONDS": "abc", "AGENT_GRAPH_RECURSION_LIMIT": "-4"}))
	if c.KubectlTimeout.Seconds() != 30 || c.GraphRecursionLimit != 120 {
		t.Error("bad numbers should fall back to defaults")
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
