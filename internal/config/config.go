package config

import (
	"bufio"
	"errors"
	"fmt"
	"math"
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

	LLMProvider string
	// LLMTemperature is 0 by default because operators want determinism, but it is
	// configurable because it cannot always be honoured: some models reject
	// temperature 0 with HTTP 400, and a hardcoded value made them unusable.
	LLMTemperature float64
	DashscopeKey   string
	QwenKey        string
	OpenAIKey      string
	OpenAIBase     string
	CoordModel     string
	SubModel       string
	AzureKey       string
	AzureEnd       string
	AzureVersion   string
	AzureCoord     string
	AzureSub       string
	AnthropicKey   string
	LargeModel     string
	SmallModel     string

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
	Reflexion             bool
	ReflexionMinConf      float64
	ReflexionVerify       bool
	ReflexionCooldownHrs  int
	ReflexionDecayDays    int
	GraphRecursionLimit   int
	Sensorium             bool
	PrometheusURL         string
	LokiURL               string
	FlightRecorder        bool
	RedactSecrets         bool
	SensoriumQueueSize    int
	SensoriumNamespaces   []string
	CoordinatorRecursions int

	// Problems are settings that could not be read as written. Each one fell
	// back to its default; Validate turns the hard ones into a startup error.
	Problems []string

	PredictiveDetection    bool
	PredictiveTrendSeconds int

	// Autonomy: A0 observe, A1 investigate and report, A2 propose, A3 auto fix
	// for allowlisted (playbook, namespace) pairs only.
	Watchtower          bool
	WatchtowerRole      string
	AutonomyLevel       string
	AutonomyNsLevels    string // "prod=A0,dev=A2"
	AutonomyA3Allowlist string // "CrashLoopBackOff/dev-*"

	// Memory
	MemoryHybrid        bool
	MemoryImportance    bool
	MemorySimFloor      float64
	PreferenceMemory    bool
	PreferenceDecayDays int
	PreferenceMinConf   float64
	PreferenceMinOccur  int
	RequireAuth         bool
	AuthBackend         string // static | hmac
	DemoKeySecret       string
	DemoKeyDefaultTTL   int // hours
	DemoKeyMaxTTL       int // hours
	MetricsEnabled      bool

	RateLimit           bool
	RateLimitPerMin     int
	RateLimitBurst      int
	RateLimitMaxTracked int
	RateLimitProxyHops  int
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
	var problems []string
	// numMin reads an integer that must be at least min. A value that is not a
	// number, or is below min, is reported and the default is used.
	numMin := func(k string, def, min int) int {
		raw := strings.TrimSpace(getenv(k))
		if raw == "" {
			return def
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s=%q is not a whole number", k, raw))
			return def
		}
		if n < min {
			problems = append(problems, fmt.Sprintf("%s=%d must be at least %d", k, n, min))
			return def
		}
		return n
	}
	num := func(k string, def int) int { return numMin(k, def, 1) }
	anyInt := func(k string, def int) int { return numMin(k, def, math.MinInt) }
	float := func(k string, def float64) float64 {
		raw := strings.TrimSpace(getenv(k))
		if raw == "" {
			return def
		}
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s=%q is not a number", k, raw))
			return def
		}
		return f
	}

	home, _ := os.UserHomeDir()
	c := &Config{
		Addr:           str("KUBESRE_ADDR", ":8000"),
		LLMProvider:    strings.ToLower(str("LLM_PROVIDER", "openai")),
		LLMTemperature: float("LLM_TEMPERATURE", 0),
		DashscopeKey:   str("DASHSCOPE_API_KEY", ""),
		QwenKey:        str("QWEN_API_KEY", ""),
		OpenAIKey:      str("OPENAI_API_KEY", ""),
		OpenAIBase:     str("OPENAI_BASE_URL", ""),
		CoordModel:     str("OPENAI_COORDINATOR_MODEL", "gpt-4o"),
		SubModel:       str("OPENAI_SUBAGENT_MODEL", "gpt-4o-mini"),
		AzureKey:       str("AZURE_OPENAI_API_KEY", ""),
		AzureEnd:       str("AZURE_OPENAI_ENDPOINT", ""),
		AzureVersion:   str("AZURE_OPENAI_API_VERSION", "2024-10-01-preview"),
		AzureCoord:     str("AZURE_COORDINATOR_DEPLOYMENT", "gpt-4o"),
		AzureSub:       str("AZURE_SUBAGENT_DEPLOYMENT", "gpt-4o-mini"),
		AnthropicKey:   str("ANTHROPIC_API_KEY", ""),
		LargeModel:     str("ANTHROPIC_LARGE_MODEL", "claude-sonnet-4-6"),
		SmallModel:     str("ANTHROPIC_SMALL_MODEL", "claude-haiku-4-5-20251001"),

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
		KubectlTimeout:      time.Duration(anyInt("KUBECTL_TIMEOUT_SECONDS", 30)) * time.Second,
		KubectlWriteTimeout: time.Duration(anyInt("KUBECTL_DESTRUCTIVE_TIMEOUT_SECONDS", 300)) * time.Second,

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
		SnapshotFreshness:     time.Duration(anyInt("SNAPSHOT_FRESHNESS_SECONDS", 30)) * time.Second,
		InvestigationPlan:     boolean("INVESTIGATION_PLAN_ENABLED", true),
		Playbooks:             boolean("PLAYBOOKS_ENABLED", true),
		Reflexion:             boolean("REFLEXION_ENABLED", true),
		ReflexionMinConf:      float("REFLEXION_MIN_CONFIDENCE", 0.7),
		ReflexionVerify:       boolean("REFLEXION_VERIFY_RESOLUTION", true),
		ReflexionCooldownHrs:  anyInt("REFLEXION_PATTERN_COOLDOWN_HOURS", 1),
		ReflexionDecayDays:    anyInt("REFLEXION_PATTERN_DECAY_DAYS", 30),
		GraphRecursionLimit:   num("AGENT_GRAPH_RECURSION_LIMIT", 120),
		Sensorium:             boolean("SENSORIUM_ENABLED", true),
		PrometheusURL:         str("PROMETHEUS_URL", ""),
		LokiURL:               str("LOKI_URL", ""),
		FlightRecorder:        boolean("FLIGHT_RECORDER_ENABLED", true),
		RedactSecrets:         boolean("REFLEXION_REDACT_SECRETS", true),
		SensoriumQueueSize:    anyInt("SENSORIUM_QUEUE_MAXSIZE", 10000),
		SensoriumNamespaces:   list(str("SENSORIUM_WATCH_NAMESPACES", "")),
		CoordinatorRecursions: num("AGENT_COORDINATOR_RECURSION_LIMIT", 150),

		PredictiveDetection:    boolean("PREDICTIVE_DETECTION_ENABLED", false),
		PredictiveTrendSeconds: anyInt("PREDICTIVE_TREND_INTERVAL_SECONDS", 60),

		Watchtower:          boolean("WATCHTOWER_ENABLED", true),
		WatchtowerRole:      str("WATCHTOWER_ROLE", "operator"),
		AutonomyLevel:       str("AUTONOMY_LEVEL", "A1"),
		AutonomyNsLevels:    str("AUTONOMY_NAMESPACE_LEVELS", ""),
		AutonomyA3Allowlist: str("AUTONOMY_A3_ALLOWLIST", ""),

		MemoryHybrid:     boolean("MEMORY_HYBRID_RETRIEVAL", false),
		MemoryImportance: boolean("MEMORY_IMPORTANCE", false),
		MemorySimFloor:   float("MEMORY_RECALL_SIMILARITY_FLOOR", 0.02),

		PreferenceMemory:    boolean("PREFERENCE_MEMORY_ENABLED", true),
		PreferenceDecayDays: anyInt("PREFERENCE_DECAY_DAYS", 60),
		PreferenceMinConf:   float("PREFERENCE_MIN_CONFIDENCE", 0.3),
		PreferenceMinOccur:  anyInt("PREFERENCE_INFER_MIN_OCCURRENCE", 3),

		RequireAuth:       boolean("REQUIRE_AUTH", false),
		AuthBackend:       strings.ToLower(str("AUTH_BACKEND", "static")),
		DemoKeySecret:     str("DEMO_KEY_HMAC_SECRET", ""),
		DemoKeyDefaultTTL: num("DEMO_KEY_DEFAULT_TTL_HOURS", 24*7),
		DemoKeyMaxTTL:     num("DEMO_KEY_MAX_TTL_HOURS", 24*30),
		MetricsEnabled:    boolean("METRICS_ENABLED", true),

		RateLimit:           boolean("RATE_LIMIT_ENABLED", true),
		RateLimitPerMin:     anyInt("RATE_LIMIT_PER_MIN", 120),
		RateLimitBurst:      anyInt("RATE_LIMIT_BURST", 30),
		RateLimitMaxTracked: anyInt("RATE_LIMIT_MAX_TRACKED", 10000),
		RateLimitProxyHops:  anyInt("RATE_LIMIT_TRUSTED_PROXY_HOPS", 0),
	}

	nsDefault := "kube-sre,monitoring,kube-system,kube-public,kube-node-lease,ingress-nginx,cert-manager"
	c.BlockedNamespaces = toSet(lower(list(str("KUBECTL_BLOCKED_NAMESPACES", nsDefault))))

	// Configuring this replaces the list, so the credential types are re-added
	// here and cannot be configured away. Singular and plural forms are expanded
	// where the names are matched, on both sides of the comparison.
	res := lower(list(str("KUBECTL_BLOCKED_RESOURCES", strings.Join(alwaysBlocked, ","))))
	c.BlockedResources = toSet(append(res, alwaysBlocked...))

	// Provider aliases: qwen is the OpenAI compatible DashScope endpoint with
	// qwen model defaults, so a Qwen deployment needs only the provider and a key.
	if c.LLMProvider == "qwen" {
		if c.OpenAIKey == "" {
			c.OpenAIKey = firstNonEmpty(c.DashscopeKey, c.QwenKey)
		}
		if c.OpenAIBase == "" {
			c.OpenAIBase = "https://dashscope-intl.aliyuncs.com/compatible-mode/v1"
		}
		if c.CoordModel == "gpt-4o" {
			c.CoordModel = "qwen-max"
		}
		if c.SubModel == "gpt-4o-mini" {
			c.SubModel = "qwen-plus"
		}
	}
	c.Problems = problems
	return c
}

func firstNonEmpty(items ...string) string {
	for _, s := range items {
		if s != "" {
			return s
		}
	}
	return ""
}

// Validate returns an error for a setting the server cannot start with: an
// unknown provider or snapshot mode, or a number that could not be read.
func (c *Config) Validate() error {
	var errs []string
	switch c.LLMProvider {
	case "azure", "openai", "anthropic", "qwen":
	default:
		errs = append(errs, fmt.Sprintf("LLM_PROVIDER must be one of azure, openai, anthropic, qwen, got %q", c.LLMProvider))
	}
	switch c.AuthBackend {
	case "static", "hmac":
	default:
		errs = append(errs, fmt.Sprintf("AUTH_BACKEND must be static or hmac, got %q", c.AuthBackend))
	}
	switch c.SnapshotMode {
	case "off", "lenient", "strict":
	default:
		errs = append(errs, fmt.Sprintf("SNAPSHOT_SUFFICIENCY_MODE must be one of off, lenient, strict, got %q", c.SnapshotMode))
	}
	errs = append(errs, c.Problems...)
	if len(errs) == 0 {
		return nil
	}
	return errors.New(strings.Join(errs, "; "))
}

// LLMWarnings lists provider settings that will make model calls fail.
func (c *Config) LLMWarnings() []string {
	var w []string
	switch c.LLMProvider {
	case "azure":
		var missing []string
		if c.AzureKey == "" {
			missing = append(missing, "AZURE_OPENAI_API_KEY")
		}
		if c.AzureEnd == "" {
			missing = append(missing, "AZURE_OPENAI_ENDPOINT")
		}
		if len(missing) > 0 {
			w = append(w, "LLM_PROVIDER=azure but missing: "+strings.Join(missing, ", ")+". LLM calls will fail until these are set.")
		}
	case "openai", "qwen":
		if c.OpenAIKey == "" {
			w = append(w, fmt.Sprintf("LLM_PROVIDER=%s but OPENAI_API_KEY is not set, so LLM calls will fail.", c.LLMProvider))
		}
	case "anthropic":
		if c.AnthropicKey == "" {
			w = append(w, "LLM_PROVIDER=anthropic but ANTHROPIC_API_KEY is not set, so LLM calls will fail.")
		}
	}
	return w
}

// FromEnvironment loads settings the way the server does: real environment
// variables first, then ./.env, then ~/.kube-sre/.env. Names are matched case
// insensitively.
func FromEnvironment() *Config {
	files := map[string]string{}
	home, _ := os.UserHomeDir()
	// Later files win, so the home file is read first.
	for _, path := range []string{filepath.Join(home, ".kube-sre", ".env"), ".env"} {
		for k, v := range readDotenv(path) {
			files[strings.ToUpper(k)] = v
		}
	}
	return Load(func(k string) string {
		if v, ok := os.LookupEnv(k); ok {
			return v
		}
		return files[strings.ToUpper(k)]
	})
}

// readDotenv parses KEY=VALUE lines, ignoring comments and blank lines and
// stripping `export ` and matching quotes.
func readDotenv(path string) map[string]string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		} else if i := strings.Index(v, " #"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		out[strings.TrimSpace(k)] = v
	}
	return out
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

// OpenAccess reports whether no auth is configured at all, so every caller is
// treated as admin.
func (c *Config) OpenAccess() bool {
	return len(c.SuperadminKeys)+len(c.AdminKeys)+len(c.OperatorKeys)+len(c.ReadonlyKeys) == 0 && c.DemoKeySecret == ""
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
