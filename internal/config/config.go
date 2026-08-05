package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr string

	LLMProvider  string
	OpenAIKey    string
	OpenAIBase   string
	CoordModel   string
	SubModel     string
	AzureKey     string
	AzureEnd     string
	AzureVersion string
	AzureCoord   string
	AzureSub     string
	AnthropicKey string
	LargeModel   string
	SmallModel   string

	DatabaseURL string
	PGHost      string
	PGPort      string
	PGDatabase  string
	PGUser      string
	PGPassword  string
	PGPoolMin   int
	PGPoolMax   int
	UseSQLite   bool
	SQLitePath  string

	KubeconfigPath      string
	ClusterID           string
	KubectlTimeout      time.Duration
	KubectlWriteTimeout time.Duration
	BlockedNamespaces   map[string]bool
	BlockedResources    map[string]bool

	SuperadminKeys []string
	AdminKeys      []string
	OperatorKeys   []string
	ReadonlyKeys   []string

	LogLevel       string
	LogFormat      string
	Debug          bool
	AllowedOrigins []string

	ErrorHints            bool
	SnapshotMode          string
	SnapshotFreshness     time.Duration
	InvestigationPlan     bool
	Playbooks             bool
	GraphRecursionLimit   int
	Sensorium             bool
	FlightRecorder        bool
	RedactSecrets         bool
	SensoriumQueueSize    int
	SensoriumNamespaces   []string
	CoordinatorRecursions int
}

var alwaysBlocked = []string{"secret", "secrets", "serviceaccount", "serviceaccounts"}

var label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func Load(getenv func(string) string) *Config {
	str := func(k, def string) string {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v
		}
		return def
	}
	boolean := func(k string, def bool) bool {
		switch strings.ToLower(strings.TrimSpace(getenv(k))) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
		return def
	}
	num := func(k string, def int) int {
		if n, err := strconv.Atoi(strings.TrimSpace(getenv(k))); err == nil && n > 0 {
			return n
		}
		return def
	}

	home, _ := os.UserHomeDir()
	c := &Config{
		Addr:         str("KUBESRE_ADDR", ":8000"),
		LLMProvider:  strings.ToLower(str("LLM_PROVIDER", "azure")),
		OpenAIKey:    str("OPENAI_API_KEY", ""),
		OpenAIBase:   str("OPENAI_BASE_URL", ""),
		CoordModel:   str("OPENAI_COORDINATOR_MODEL", "gpt-4o"),
		SubModel:     str("OPENAI_SUBAGENT_MODEL", "gpt-4o-mini"),
		AzureKey:     str("AZURE_OPENAI_API_KEY", ""),
		AzureEnd:     str("AZURE_OPENAI_ENDPOINT", ""),
		AzureVersion: str("AZURE_OPENAI_API_VERSION", "2024-10-01-preview"),
		AzureCoord:   str("AZURE_COORDINATOR_DEPLOYMENT", "gpt-4o"),
		AzureSub:     str("AZURE_SUBAGENT_DEPLOYMENT", "gpt-4o-mini"),
		AnthropicKey: str("ANTHROPIC_API_KEY", ""),
		LargeModel:   str("ANTHROPIC_LARGE_MODEL", "claude-sonnet-4-6"),
		SmallModel:   str("ANTHROPIC_SMALL_MODEL", "claude-haiku-4-5-20251001"),

		DatabaseURL: str("DATABASE_URL", ""),
		PGHost:      str("POSTGRES_HOST", ""),
		PGPort:      str("POSTGRES_PORT", "5432"),
		PGDatabase:  str("POSTGRES_DB", "kube-sre"),
		PGUser:      str("POSTGRES_USER", "kube-sre"),
		PGPassword:  str("POSTGRES_PASSWORD", "password"),
		PGPoolMin:   num("POSTGRES_POOL_MIN_CONN", 1),
		PGPoolMax:   num("POSTGRES_POOL_MAX_CONN", 10),
		UseSQLite:   boolean("USE_SQLITE", false),
		SQLitePath:  str("SQLITE_PATH", filepath.Join(home, ".kube-sre", "kube-sre.db")),

		KubeconfigPath:      str("KUBECONFIG_PATH", filepath.Join(home, ".kube", "config")),
		ClusterID:           str("CLUSTER_ID", ""),
		KubectlTimeout:      time.Duration(num("KUBECTL_TIMEOUT_SECONDS", 30)) * time.Second,
		KubectlWriteTimeout: time.Duration(num("KUBECTL_DESTRUCTIVE_TIMEOUT_SECONDS", 300)) * time.Second,

		SuperadminKeys: list(getenv("KUBESRE_SUPERADMIN_KEYS")),
		AdminKeys:      list(getenv("KUBESRE_ADMIN_KEYS")),
		OperatorKeys:   list(getenv("KUBESRE_OPERATOR_KEYS")),
		ReadonlyKeys:   list(getenv("KUBESRE_READONLY_KEYS")),

		LogLevel:       strings.ToUpper(str("LOG_LEVEL", "INFO")),
		LogFormat:      strings.ToLower(str("LOG_FORMAT", "text")),
		Debug:          boolean("DEBUG", false),
		AllowedOrigins: list(str("ALLOWED_ORIGINS", "http://localhost:3080")),

		ErrorHints:            boolean("KUBECTL_ERROR_HINTS_ENABLED", true),
		SnapshotMode:          strings.ToLower(str("SNAPSHOT_SUFFICIENCY_MODE", "lenient")),
		SnapshotFreshness:     time.Duration(num("SNAPSHOT_FRESHNESS_SECONDS", 30)) * time.Second,
		InvestigationPlan:     boolean("INVESTIGATION_PLAN_ENABLED", true),
		Playbooks:             boolean("PLAYBOOKS_ENABLED", true),
		GraphRecursionLimit:   num("AGENT_GRAPH_RECURSION_LIMIT", 120),
		Sensorium:             boolean("SENSORIUM_ENABLED", true),
		FlightRecorder:        boolean("FLIGHT_RECORDER_ENABLED", true),
		RedactSecrets:         boolean("REFLEXION_REDACT_SECRETS", true),
		SensoriumQueueSize:    num("SENSORIUM_QUEUE_MAXSIZE", 10000),
		SensoriumNamespaces:   list(str("SENSORIUM_WATCH_NAMESPACES", "")),
		CoordinatorRecursions: num("AGENT_COORDINATOR_RECURSION_LIMIT", 150),
	}

	nsDefault := "kube-sre,monitoring,kube-system,kube-public,kube-node-lease,ingress-nginx,cert-manager"
	c.BlockedNamespaces = toSet(lower(list(str("KUBECTL_BLOCKED_NAMESPACES", nsDefault))))

	res := lower(list(str("KUBECTL_BLOCKED_RESOURCES", strings.Join(alwaysBlocked, ","))))
	c.BlockedResources = toSet(append(spellings(res), alwaysBlocked...))
	return c
}

// Postgres reports whether the server should use Postgres instead of SQLite.
func (c *Config) Postgres() bool {
	if c.UseSQLite {
		return false
	}
	return c.DatabaseURL != "" || c.PGHost != ""
}

// DSN returns the Postgres connection string.
func (c *Config) DSN() string {
	if c.DatabaseURL != "" {
		return c.DatabaseURL
	}
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(c.PGUser, c.PGPassword),
		Host:   c.PGHost + ":" + c.PGPort,
		Path:   "/" + c.PGDatabase,
	}
	return u.String()
}

// OpenAccess reports whether no auth keys are configured at all.
func (c *Config) OpenAccess() bool {
	return len(c.SuperadminKeys)+len(c.AdminKeys)+len(c.OperatorKeys)+len(c.ReadonlyKeys) == 0
}

// Warnings lists guard settings that can never match anything.
func (c *Config) Warnings() []string {
	var w []string
	for ns := range c.BlockedNamespaces {
		if !label.MatchString(ns) {
			w = append(w, fmt.Sprintf("blocked namespace %q is not a valid name and protects nothing", ns))
		}
	}
	for _, o := range c.AllowedOrigins {
		switch {
		case o == "*":
			w = append(w, "allowed origin * lets any site call this api with a session")
		case !strings.Contains(o, "://"):
			w = append(w, fmt.Sprintf("allowed origin %q has no scheme and can never match", o))
		case strings.HasSuffix(o, "/"):
			w = append(w, fmt.Sprintf("allowed origin %q has a trailing slash and can never match", o))
		}
	}
	return w
}

func list(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func lower(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(s)
	}
	return out
}

func toSet(in []string) map[string]bool {
	m := make(map[string]bool, len(in))
	for _, s := range in {
		m[s] = true
	}
	return m
}

// spellings treats singular and plural forms of a resource as one entry.
func spellings(in []string) []string {
	var out []string
	for _, s := range in {
		out = append(out, s, s+"s", s+"es")
		if t := strings.TrimSuffix(s, "es"); t != s {
			out = append(out, t)
		}
		if t := strings.TrimSuffix(s, "s"); t != s {
			out = append(out, t)
		}
	}
	return out
}
