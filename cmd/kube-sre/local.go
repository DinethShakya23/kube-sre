package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/client"
	"github.com/DinethShakya23/kube-sre/internal/config"
	"github.com/DinethShakya23/kube-sre/internal/provenance"
	"github.com/DinethShakya23/kube-sre/internal/setup"
	"github.com/DinethShakya23/kube-sre/internal/store"
)

func sh(argv ...string) (bool, string) {
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	return err == nil, strings.TrimSpace(string(out))
}

func exists(bin string) bool { _, err := exec.LookPath(bin); return err == nil }

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func expand(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

// statusCmd is the local health dashboard. It exits non zero when a component failed.
func statusCmd(args []string) int {
	fs, server, key, user := clientFlags("status")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg := config.FromEnvironment()
	home := setup.ConfigFile()
	var files []string
	if fileExists(home) {
		files = append(files, home)
	}
	if abs, _ := filepath.Abs(".env"); fileExists(".env") && abs != home {
		files = append(files, abs+" (working directory)")
	}
	model := cfg.CoordModel
	if cfg.LLMProvider == "azure" {
		model = cfg.AzureCoord
	}
	s := setup.Settings{
		ConfigFiles: files, HomeFile: home, Provider: cfg.LLMProvider, Model: model, MissingLLM: llmMissing(cfg),
		UseSQLite: !cfg.Postgres(), DBLabel: dbLabel(cfg), Kubeconfig: expand(cfg.KubeconfigPath), OpenAccess: cfg.OpenAccess(),
		AdminKeys: !cfg.OpenAccess(),
	}
	s.DBFileFound = fileExists(cfg.SQLitePath)
	s.KubeExists = fileExists(s.Kubeconfig)
	c := client.New(*server, *key, *user)
	probes := setup.Probes{
		Exists: exists,
		Cluster: func(kc string) (bool, string, error) {
			ctxOut := ""
			if ok, out := sh("kubectl", "--kubeconfig", kc, "config", "current-context"); ok {
				ctxOut = out
			}
			ok, _ := sh("kubectl", "--kubeconfig", kc, "--request-timeout=5s", "get", "--raw", "/readyz")
			return ok, ctxOut, nil
		},
		Server: func() (bool, string, error) {
			cctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			text, ok, err := c.Status(cctx)
			if err != nil {
				return false, "", err
			}
			first, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
			return ok, strings.Join(strings.Fields(first), " "), nil
		},
		DBCheck: func() error {
			db, err := store.Open(cfg)
			if err != nil {
				return err
			}
			defer db.Close()
			return db.Ping()
		},
	}
	r := setup.Build(s, probes)
	fmt.Print(r.Render())
	if !r.OK() {
		return 1
	}
	return 0
}

func llmMissing(cfg *config.Config) []string {
	var miss []string
	switch cfg.LLMProvider {
	case "azure":
		if cfg.AzureKey == "" {
			miss = append(miss, "AZURE_OPENAI_API_KEY")
		}
		if cfg.AzureEnd == "" {
			miss = append(miss, "AZURE_OPENAI_ENDPOINT")
		}
	case "anthropic":
		if cfg.AnthropicKey == "" {
			miss = append(miss, "ANTHROPIC_API_KEY")
		}
	case "qwen":
		if cfg.DashscopeKey == "" && cfg.QwenKey == "" {
			miss = append(miss, "DASHSCOPE_API_KEY")
		}
	default:
		if cfg.OpenAIKey == "" {
			miss = append(miss, "OPENAI_API_KEY")
		}
	}
	return miss
}

func dbLabel(cfg *config.Config) string {
	if !cfg.Postgres() {
		return cfg.SQLitePath
	}
	if cfg.PGHost != "" {
		return cfg.PGHost + "/" + cfg.PGDatabase
	}
	return "DATABASE_URL"
}

// healthCmd prints the running server's subsystem blocks.
func healthCmd(args []string) int {
	fs, server, key, user := clientFlags("health")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	text, ok, err := client.New(*server, *key, *user).Status(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Print(text)
	if !ok {
		return 1
	}
	return 0
}

// setCmd sets configuration values in ~/.kube-sre/.env.
func setCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: kube-sre set KEY=VALUE [KEY=VALUE ...]")
		return 2
	}
	changed, err := setup.SetValues(setup.ConfigFile(), args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	for _, c := range changed {
		fmt.Printf("  ✓  %s = %s\n", c.Key, c.Shown)
	}
	// Reload the service if it is running, so the values take effect now. This line is
	// the user's only signal that the running server picked them up, so it must not
	// appear when the restart did not happen.
	if active, _ := sh("systemctl", "--user", "is-active", setup.ServiceName); active {
		if ok, detail := sh("systemctl", "--user", "restart", setup.ServiceName); ok {
			fmt.Println("  → service restarted to apply changes")
		} else {
			fmt.Println("  ⚠  The service is running but would not restart — it is still using the previous configuration.")
			if detail != "" {
				fmt.Println("     systemctl:", detail)
			}
			fmt.Printf("     Retry with: systemctl --user restart %s\n", setup.ServiceName)
		}
	}
	return 0
}

func ask(in *bufio.Reader, prompt, def string) string {
	if def != "" {
		fmt.Printf("  %s [%s]: ", prompt, def)
	} else {
		fmt.Printf("  %s: ", prompt)
	}
	line, _ := in.ReadString('\n')
	if line = strings.TrimSpace(line); line != "" {
		return line
	}
	return def
}

// initCmd writes a first configuration. Flags make it scriptable; anything missing is asked.
func initCmd(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	prov := fs.String("provider", "", "openai, azure, anthropic or qwen")
	apiKey := fs.String("api-key", "", "the model API key")
	endpoint := fs.String("endpoint", "", "azure endpoint")
	pg := fs.String("postgres", "", "a Postgres DSN (default: SQLite)")
	yes := fs.Bool("yes", false, "never prompt; fail if something is missing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	in := bufio.NewReader(os.Stdin)
	a := setup.Answers{Provider: *prov, APIKey: *apiKey, Endpoint: *endpoint, Postgres: *pg}
	if !*yes {
		fmt.Print("\n  kube-sre setup\n\n")
		a.Provider = ask(in, "Model provider (openai, azure, anthropic, qwen)", firstOf(a.Provider, "openai"))
		a.APIKey = ask(in, "API key", a.APIKey)
		if strings.EqualFold(a.Provider, "azure") {
			a.Endpoint = ask(in, "Azure endpoint", a.Endpoint)
		}
		a.Postgres = ask(in, "Postgres DSN (empty for SQLite)", a.Postgres)
	}
	assignments, adminKey, err := a.Assignments()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if _, err := setup.SetValues(setup.ConfigFile(), assignments); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Printf("\n  Wrote %s\n  Admin API key (shown once): %s\n\n  Next:  kube-sre serve\n         kube-sre chat --key %s\n\n", setup.ConfigFile(), adminKey, adminKey)
	return 0
}

func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func serviceCmd(args []string) int {
	if len(args) != 1 || (args[0] != "install" && args[0] != "uninstall" && args[0] != "status") {
		fmt.Fprintln(os.Stderr, "usage: kube-sre service install|uninstall|status")
		return 2
	}
	if runtime.GOOS != "linux" {
		fmt.Fprintln(os.Stderr, "the service commands manage a systemd user unit and only work on Linux; run `kube-sre serve` under your own supervisor here")
		return 1
	}
	home, _ := os.UserHomeDir()
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	switch args[0] {
	case "install":
		bin, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		wd, _ := os.Getwd()
		if res := setup.InstallService(unitDir, bin, wd, sh); !res.OK {
			fmt.Fprintln(os.Stderr, "service install failed:", res.Detail)
			return 1
		} else {
			fmt.Println("  ✓  installed and started:", res.Detail)
		}
	case "uninstall":
		if res := setup.UninstallService(unitDir, sh); !res.OK {
			fmt.Fprintln(os.Stderr, "service uninstall failed:", res.Detail)
			return 1
		}
		fmt.Println("  ✓  removed")
	default:
		_, out := sh("systemctl", "--user", "status", setup.ServiceName, "--no-pager")
		fmt.Println(out)
	}
	return 0
}

func kindSetupCmd(args []string) int {
	fs := flag.NewFlagSet("kind-setup", flag.ContinueOnError)
	name := fs.String("name", "kube-sre", "cluster name")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	res := setup.KindSetup(*name, exists, sh)
	for _, t := range res.Missing {
		fmt.Fprintf(os.Stderr, "  ✗  %s is not installed: %s\n", t.Name, t.Hint)
	}
	if len(res.Missing) > 0 || (!res.Created && !strings.Contains(res.Detail, "already exists")) {
		fmt.Fprintln(os.Stderr, " ", res.Detail)
		return 1
	}
	fmt.Println("  ✓ ", res.Detail)
	return 0
}

func provenanceCmd(args []string) int {
	fs := flag.NewFlagSet("provenance", flag.ContinueOnError)
	tag := fs.String("tag", "v"+versionString(), "release tag to verify")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fmt.Print(provenance.Report(*tag))
	return 0
}
