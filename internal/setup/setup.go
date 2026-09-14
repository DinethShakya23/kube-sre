// Package setup is what the command line does on the machine rather than through the
// API: writing the configuration file, the health dashboard, the systemd service and the
// local cluster bootstrap. Every step that touches the outside world takes its probe or
// runner as a value, so the logic is testable without a cluster, a database or root.
package setup

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ConfigDir is ~/.kube-sre.
func ConfigDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kube-sre")
}

// ConfigFile is ~/.kube-sre/.env.
func ConfigFile() string { return filepath.Join(ConfigDir(), ".env") }

var secretWords = []string{"KEY", "SECRET", "PASSWORD", "TOKEN"}

// Mask hides secret values when echoing a setting back. Values are still written in
// full; only what is printed is masked.
func Mask(key, value string) string {
	up := strings.ToUpper(key)
	for _, w := range secretWords {
		if strings.Contains(up, w) {
			if len(value) <= 8 {
				return "****"
			}
			return value[:4] + "…" + value[len(value)-2:]
		}
	}
	return value
}

// Change is one setting written to the file.
type Change struct{ Key, Value, Shown string }

// SetValues sets KEY=VALUE assignments in an env file, replacing an existing line or
// appending. The file is created with owner only permissions, since it holds keys. An
// assignment without an equals sign or with an empty key is an error and changes nothing.
func SetValues(path string, assignments []string) ([]Change, error) {
	type pair struct{ k, v string }
	var pairs []pair
	for _, a := range assignments {
		k, v, ok := strings.Cut(a, "=")
		k = strings.TrimSpace(k)
		switch {
		case !ok:
			return nil, fmt.Errorf("invalid argument %q — expected KEY=VALUE", a)
		case k == "":
			return nil, fmt.Errorf("empty key in %q", a)
		}
		pairs = append(pairs, pair{k, v})
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	var lines []string
	if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
		lines = strings.SplitAfter(string(b), "\n")
		if lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
	}
	var changed []Change
	for _, p := range pairs {
		line := p.k + "=" + p.v + "\n"
		replaced := false
		for i, l := range lines {
			if strings.HasPrefix(l, p.k+"=") || strings.HasPrefix(l, p.k+" =") {
				lines[i], replaced = line, true
				break
			}
		}
		if !replaced {
			if n := len(lines); n > 0 && !strings.HasSuffix(lines[n-1], "\n") {
				lines[n-1] += "\n"
			}
			lines = append(lines, line)
		}
		changed = append(changed, Change{p.k, p.v, Mask(p.k, p.v)})
	}
	return changed, os.WriteFile(path, []byte(strings.Join(lines, "")), 0o600)
}

// ── init ─────────────────────────────────────────────────────────────────────

// Answers are what the init wizard collects.
type Answers struct {
	Provider   string // openai | azure | anthropic | qwen
	APIKey     string
	BaseURL    string // openai compatible endpoint, optional
	Endpoint   string // azure
	Deployment string // azure coordinator deployment
	Model      string // coordinator model, optional
	Postgres   string // a DSN; empty means SQLite
	AdminKey   string // generated when empty
}

// NewKey makes an API key for the admin role.
func NewKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "sre_" + hex.EncodeToString(b)
}

// Assignments turns answers into the settings the wizard writes, plus the admin key
// it generated. Nothing is guessed: a provider that needs an endpoint and has none is
// an error, so init never writes a configuration that cannot start.
func (a Answers) Assignments() ([]string, string, error) {
	prov := strings.ToLower(strings.TrimSpace(a.Provider))
	if prov == "" {
		prov = "openai"
	}
	out := []string{"LLM_PROVIDER=" + prov}
	switch prov {
	case "openai":
		out = append(out, "OPENAI_API_KEY="+a.APIKey)
		if a.BaseURL != "" {
			out = append(out, "OPENAI_BASE_URL="+a.BaseURL)
		}
		if a.Model != "" {
			out = append(out, "OPENAI_COORDINATOR_MODEL="+a.Model)
		}
	case "azure":
		if a.Endpoint == "" {
			return nil, "", fmt.Errorf("azure needs an endpoint")
		}
		out = append(out, "AZURE_OPENAI_API_KEY="+a.APIKey, "AZURE_OPENAI_ENDPOINT="+a.Endpoint)
		if a.Deployment != "" {
			out = append(out, "AZURE_COORDINATOR_DEPLOYMENT="+a.Deployment)
		}
	case "anthropic":
		out = append(out, "ANTHROPIC_API_KEY="+a.APIKey)
	case "qwen":
		out = append(out, "DASHSCOPE_API_KEY="+a.APIKey)
	default:
		return nil, "", fmt.Errorf("unknown provider %q — use openai, azure, anthropic or qwen", a.Provider)
	}
	if strings.TrimSpace(a.APIKey) == "" {
		return nil, "", fmt.Errorf("an API key is required for %s", prov)
	}
	if a.Postgres != "" {
		out = append(out, "DATABASE_URL="+a.Postgres)
	} else {
		out = append(out, "USE_SQLITE=true")
	}
	key := a.AdminKey
	if key == "" {
		key = NewKey()
	}
	out = append(out, "KUBESRE_ADMIN_KEYS="+key)
	return out, key, nil
}

// ── the systemd service ──────────────────────────────────────────────────────

const ServiceName = "kube-sre"

// ServiceUnit is the user level systemd unit. It runs the binary as the user, so it
// reads the same ~/.kube-sre/.env as an interactive run, and restarts on failure.
func ServiceUnit(binary, workdir string) string {
	return fmt.Sprintf(`[Unit]
Description=kube-sre
After=network-online.target

[Service]
Type=simple
WorkingDirectory=%s
ExecStart=%s serve
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, workdir, binary)
}

// Runner runs a command and returns whether it succeeded and its output.
type Runner func(argv ...string) (bool, string)

// ServiceResult is what an install or uninstall did, and why it stopped if it did.
type ServiceResult struct {
	OK     bool
	Detail string
}

// InstallService writes the unit and enables it. Each step reports its own failure
// with the command's output, so a user is never told "failed" with no reason.
func InstallService(unitDir, binary, workdir string, run Runner) ServiceResult {
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return ServiceResult{false, "could not create " + unitDir + ": " + err.Error()}
	}
	path := filepath.Join(unitDir, ServiceName+".service")
	if err := os.WriteFile(path, []byte(ServiceUnit(binary, workdir)), 0o644); err != nil {
		return ServiceResult{false, "could not write " + path + ": " + err.Error()}
	}
	for _, step := range [][]string{{"systemctl", "--user", "daemon-reload"}, {"systemctl", "--user", "enable", "--now", ServiceName}} {
		if ok, out := run(step...); !ok {
			return ServiceResult{false, strings.Join(step, " ") + " failed: " + out}
		}
	}
	return ServiceResult{true, path}
}

// UninstallService stops, disables and removes the unit.
func UninstallService(unitDir string, run Runner) ServiceResult {
	run("systemctl", "--user", "disable", "--now", ServiceName)
	path := filepath.Join(unitDir, ServiceName+".service")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return ServiceResult{false, "could not remove " + path + ": " + err.Error()}
	}
	run("systemctl", "--user", "daemon-reload")
	return ServiceResult{true, path}
}

// ── the health dashboard ─────────────────────────────────────────────────────

// Row states.
const (
	Good = "ok"
	Bad  = "fail"
	Note = "warn" // not configured: a choice, not a fault
)

// Row is one line of the dashboard.
type Row struct{ Label, State, Text string }

// Report is the dashboard. Failures names every component in the failed state, so the
// exit code can say what the board says: status is the obvious thing to put in a
// Makefile or a container health check, and it exited 0 with an unreachable database,
// a dead API server and a missing key on screen, so none of those uses could ever fail.
// A warn row is not counted.
type Report struct {
	Rows     []Row
	Failures []string
}

func (r *Report) add(label, state, text string) {
	r.Rows = append(r.Rows, Row{label, state, text})
	if state == Bad {
		r.Failures = append(r.Failures, strings.ToLower(strings.TrimSuffix(label, ":")))
	}
}

// OK is true when nothing failed.
func (r Report) OK() bool { return len(r.Failures) == 0 }

// Probes are the facts the dashboard cannot know on its own.
type Probes struct {
	Exists  func(binary string) bool
	Cluster func(kubeconfig string) (reachable bool, context string, err error)
	Server  func() (ok bool, detail string, err error)
	DBCheck func() error
}

// Settings is the slice of configuration the dashboard reports on.
type Settings struct {
	ConfigFiles []string // files that exist and were read
	HomeFile    string
	Provider    string
	Model       string
	HasLLMKey   bool
	MissingLLM  []string
	UseSQLite   bool
	DBLabel     string
	DBFileFound bool
	Kubeconfig  string
	KubeExists  bool
	OpenAccess  bool
	AdminKeys   bool
}

// Build assembles the dashboard. The LLM row checks that settings are present and says
// so: it makes no request, since even the cheapest probe is a round trip to a paid
// endpoint and status is run casually and often. What it must not do is print a bare tick
// that reads as "the model is usable", when a revoked key, a mistyped endpoint and a
// deployment that does not exist all look identical from here.
func Build(s Settings, p Probes) Report {
	var r Report
	if len(s.ConfigFiles) > 0 {
		r.add("Config:", Good, strings.Join(s.ConfigFiles, ", "))
	} else {
		r.add("Config:", Bad, s.HomeFile+"  → run: kube-sre init")
	}
	switch {
	case len(s.MissingLLM) > 0:
		r.add("LLM:", Bad, fmt.Sprintf("%s / %s  missing: %s", s.Provider, s.Model, strings.Join(s.MissingLLM, ", ")))
	default:
		r.add("LLM:", Good, fmt.Sprintf("%s / %s  configured — not verified, no request is made", s.Provider, s.Model))
	}
	if s.UseSQLite {
		if s.DBFileFound {
			r.add("DB:", Good, "sqlite  "+s.DBLabel)
		} else {
			r.add("DB:", Note, "sqlite  "+s.DBLabel+"  (will be created on first start)")
		}
	} else if err := p.DBCheck(); err != nil {
		r.add("DB:", Bad, "postgres  "+s.DBLabel+"  unreachable: "+err.Error())
	} else {
		r.add("DB:", Good, "postgres  "+s.DBLabel)
	}
	if p.Exists("kubectl") {
		r.add("kubectl:", Good, "found")
	} else {
		r.add("kubectl:", Bad, "not found  → run: kube-sre kind-setup")
	}
	switch {
	case !s.KubeExists:
		r.add("Cluster:", Note, s.Kubeconfig+" not found")
	default:
		up, ctx, err := p.Cluster(s.Kubeconfig)
		switch {
		case err != nil:
			r.add("Cluster:", Bad, "could not check: "+err.Error())
		case up:
			r.add("Cluster:", Good, ctx+"  reachable")
		default:
			r.add("Cluster:", Bad, ctx+"  unreachable")
		}
	}
	switch {
	case s.AdminKeys:
		r.add("Auth:", Good, "API keys configured")
	case s.OpenAccess:
		r.add("Auth:", Note, "no keys: every caller is admin (fine for local use)")
	}
	if up, detail, err := p.Server(); err != nil {
		r.add("Server:", Note, "not running")
	} else if up {
		r.add("Server:", Good, detail)
	} else {
		r.add("Server:", Bad, detail)
	}
	return r
}

// Render prints the dashboard as plain text.
func (r Report) Render() string {
	marks := map[string]string{Good: "✓", Bad: "✗", Note: "-"}
	var b strings.Builder
	b.WriteString("\n  kube-sre status\n\n")
	for _, row := range r.Rows {
		fmt.Fprintf(&b, "  %-10s %s  %s\n", row.Label, marks[row.State], row.Text)
	}
	if len(r.Failures) > 0 {
		sort.Strings(r.Failures)
		fmt.Fprintf(&b, "\n  %d problem(s): %s\n", len(r.Failures), strings.Join(r.Failures, ", "))
	}
	return b.String()
}

// ── the local cluster ────────────────────────────────────────────────────────

// Tool is a binary the local cluster needs, and how to get it.
type Tool struct{ Name, Hint string }

var kindTools = []Tool{
	{"kind", "https://kind.sigs.k8s.io/docs/user/quick-start/#installation"},
	{"kubectl", "https://kubernetes.io/docs/tasks/tools/"},
	{"docker", "https://docs.docker.com/get-docker/"},
}

// KindResult is what the bootstrap did.
type KindResult struct {
	Missing []Tool
	Created bool
	Detail  string
}

// KindSetup makes sure a local kind cluster exists. It reports every missing tool at
// once and installs nothing itself: fetching and running binaries as root is not what a
// setup command should decide on the user's behalf.
func KindSetup(name string, have func(string) bool, run Runner) KindResult {
	var res KindResult
	for _, t := range kindTools {
		if !have(t.Name) {
			res.Missing = append(res.Missing, t)
		}
	}
	if len(res.Missing) > 0 {
		res.Detail = "install the missing tools first, then run this again"
		return res
	}
	ok, out := run("kind", "get", "clusters")
	if !ok {
		res.Detail = "could not list kind clusters (is Docker running?): " + out
		return res
	}
	for _, c := range strings.Fields(out) {
		if c == name {
			res.Detail = "cluster " + name + " already exists"
			return res
		}
	}
	if ok, out = run("kind", "create", "cluster", "--name", name, "--wait", "120s"); !ok {
		res.Detail = "kind create cluster failed: " + out
		return res
	}
	res.Created, res.Detail = true, "cluster "+name+" is ready (kubectl context kind-"+name+")"
	return res
}
