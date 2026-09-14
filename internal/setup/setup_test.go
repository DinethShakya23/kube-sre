package setup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaskHidesSecretsOnly(t *testing.T) {
	for _, c := range []struct{ k, v, want string }{
		{"OPENAI_API_KEY", "sk-abcdef123456", "sk-a…56"},
		{"KUBESRE_ADMIN_KEYS", "short", "****"},
		{"POSTGRES_PASSWORD", "hunter2hunter2", "hunt…r2"},
		{"DEMO_KEY_HMAC_SECRET", "0123456789abcdef", "0123…ef"},
		{"LLM_PROVIDER", "openai", "openai"},
		{"SQLITE_PATH", "/tmp/a.db", "/tmp/a.db"},
	} {
		if got := Mask(c.k, c.v); got != c.want {
			t.Errorf("%s: %q", c.k, got)
		}
	}
}

func TestSetValuesReplacesAppendsAndProtectsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg", ".env")
	ch, err := SetValues(path, []string{"A=1", "OPENAI_API_KEY=sk-secretsecret"})
	if err != nil || len(ch) != 2 || ch[1].Shown == ch[1].Value || ch[1].Value != "sk-secretsecret" {
		t.Fatalf("%+v %v", ch, err)
	}
	if _, err = SetValues(path, []string{"A=2", "B = keep", "C=3"}); err != nil {
		t.Fatal(err)
	}
	if _, err = SetValues(path, []string{"B=new"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "A=2\nOPENAI_API_KEY=sk-secretsecret\nB=new\nC=3\n" {
		t.Errorf("%q", b)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("a file holding keys must be owner only: %v", st.Mode().Perm())
	}
	if st, _ := os.Stat(filepath.Dir(path)); st.Mode().Perm() != 0o700 {
		t.Errorf("%v", st.Mode().Perm())
	}
	// A file that does not end in a newline gets one before the append.
	_ = os.WriteFile(path, []byte("X=1"), 0o600)
	SetValues(path, []string{"Y=2"})
	if b, _ = os.ReadFile(path); string(b) != "X=1\nY=2\n" {
		t.Errorf("%q", b)
	}
	if v, _, _ := strings.Cut(string(b), "\n"); v != "X=1" {
		t.Error("existing lines are kept")
	}
}

func TestSetValuesRefusesBadInputAndChangesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	_ = os.WriteFile(path, []byte("A=1\n"), 0o600)
	for _, bad := range [][]string{{"NOEQUALS"}, {"=v"}, {"OK=1", "broken"}} {
		if _, err := SetValues(path, bad); err == nil {
			t.Errorf("%v must be refused", bad)
		}
	}
	if b, _ := os.ReadFile(path); string(b) != "A=1\n" {
		t.Errorf("nothing may be written on a bad argument: %q", b)
	}
}

func TestInitAnswersBecomeAConfigurationThatCanStart(t *testing.T) {
	got, key, err := Answers{Provider: "openai", APIKey: "sk-x", Model: "gpt-4o"}.Assignments()
	if err != nil || !strings.HasPrefix(key, "sre_") || len(key) != 36 {
		t.Fatalf("%v %v", key, err)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{"LLM_PROVIDER=openai", "OPENAI_API_KEY=sk-x", "OPENAI_COORDINATOR_MODEL=gpt-4o", "USE_SQLITE=true", "KUBESRE_ADMIN_KEYS=" + key} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in\n%s", want, joined)
		}
	}
	az, _, err := Answers{Provider: "AZURE", APIKey: "k", Endpoint: "https://x.openai.azure.com", Deployment: "gpt4", Postgres: "postgres://u:p@h/db", AdminKey: "mine"}.Assignments()
	if err != nil || !strings.Contains(strings.Join(az, "\n"), "AZURE_COORDINATOR_DEPLOYMENT=gpt4") || !strings.Contains(strings.Join(az, "\n"), "DATABASE_URL=postgres://u:p@h/db") ||
		strings.Contains(strings.Join(az, "\n"), "USE_SQLITE") || !strings.Contains(strings.Join(az, "\n"), "KUBESRE_ADMIN_KEYS=mine") {
		t.Errorf("%v %v", az, err)
	}
	for name, a := range map[string]Answers{
		"no key":            {Provider: "openai"},
		"azure no endpoint": {Provider: "azure", APIKey: "k"},
		"unknown":           {Provider: "gemini", APIKey: "k"},
	} {
		if _, _, err := a.Assignments(); err == nil {
			t.Errorf("%s must be refused: init never writes a configuration that cannot start", name)
		}
	}
	if a, b := NewKey(), NewKey(); a == b {
		t.Error("keys are random")
	}
	for _, p := range []string{"qwen", "anthropic"} {
		if got, _, err := (Answers{Provider: p, APIKey: "k"}).Assignments(); err != nil || len(got) < 4 {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestServiceUnitAndInstallStopWithTheReason(t *testing.T) {
	unit := ServiceUnit("/usr/local/bin/kube-sre", "/home/u")
	for _, want := range []string{"ExecStart=/usr/local/bin/kube-sre serve", "WorkingDirectory=/home/u", "Restart=on-failure", "WantedBy=default.target"} {
		if !strings.Contains(unit, want) {
			t.Errorf("missing %q", want)
		}
	}
	dir := filepath.Join(t.TempDir(), "systemd", "user")
	var ran []string
	res := InstallService(dir, "/bin/kube-sre", "/home/u", func(a ...string) (bool, string) { ran = append(ran, strings.Join(a, " ")); return true, "" })
	if !res.OK || len(ran) != 2 || ran[0] != "systemctl --user daemon-reload" || ran[1] != "systemctl --user enable --now kube-sre" {
		t.Errorf("%+v %v", res, ran)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "kube-sre.service")); !strings.Contains(string(b), "/bin/kube-sre serve") {
		t.Error("unit written")
	}
	res = InstallService(dir, "/bin/kube-sre", "/w", func(a ...string) (bool, string) { return false, "Failed to connect to bus" })
	if res.OK || !strings.Contains(res.Detail, "systemctl --user daemon-reload failed: Failed to connect to bus") {
		t.Errorf("a failure names the step and carries the output: %+v", res)
	}
	ran = nil
	if res = UninstallService(dir, func(a ...string) (bool, string) { ran = append(ran, a[2]); return true, "" }); !res.OK || len(ran) != 2 {
		t.Errorf("%+v %v", res, ran)
	}
	if _, err := os.Stat(filepath.Join(dir, "kube-sre.service")); !os.IsNotExist(err) {
		t.Error("removed")
	}
	if !UninstallService(dir, func(...string) (bool, string) { return true, "" }).OK {
		t.Error("uninstalling what is not installed is fine")
	}
}

func probes() Probes {
	return Probes{
		Exists:  func(string) bool { return true },
		Cluster: func(string) (bool, string, error) { return true, "kind-x", nil },
		Server:  func() (bool, string, error) { return true, "status ok (version 0.1.0)", nil },
		DBCheck: func() error { return nil },
	}
}

func healthy() Settings {
	return Settings{ConfigFiles: []string{"/h/.env"}, HomeFile: "/h/.env", Provider: "openai", Model: "gpt-4o", UseSQLite: true, DBLabel: "/h/db", DBFileFound: true,
		Kubeconfig: "/h/.kube/config", KubeExists: true, AdminKeys: true}
}

func TestHealthyDashboardHasNoFailures(t *testing.T) {
	r := Build(healthy(), probes())
	if !r.OK() || len(r.Rows) != 7 {
		t.Fatalf("%+v", r)
	}
	out := r.Render()
	for _, want := range []string{"Config:", "configured — not verified, no request is made", "sqlite  /h/db", "kind-x  reachable", "status ok (version 0.1.0)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "problem(s)") {
		t.Error("no problems, no summary")
	}
}

func TestEveryFailureCountsSoTheExitCodeCanSayWhatTheBoardSays(t *testing.T) {
	s := healthy()
	s.ConfigFiles = nil
	s.MissingLLM = []string{"OPENAI_API_KEY"}
	s.UseSQLite = false
	s.DBLabel = "db:5432/kubesre"
	p := probes()
	p.DBCheck = func() error { return errors.New("connection refused") }
	p.Exists = func(string) bool { return false }
	p.Cluster = func(string) (bool, string, error) { return false, "prod", nil }
	p.Server = func() (bool, string, error) { return false, "readyz says draining", nil }
	r := Build(s, p)
	if r.OK() || len(r.Failures) != 6 {
		t.Fatalf("%v", r.Failures)
	}
	out := r.Render()
	for _, want := range []string{"missing: OPENAI_API_KEY", "unreachable: connection refused", "→ run: kube-sre kind-setup", "prod  unreachable", "readyz says draining", "6 problem(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

func TestNotConfiguredIsAChoiceNotAFault(t *testing.T) {
	s := healthy()
	s.DBFileFound = false
	s.KubeExists = false
	s.AdminKeys = false
	s.OpenAccess = true
	p := probes()
	p.Server = func() (bool, string, error) { return false, "", errors.New("connection refused") }
	r := Build(s, p)
	if !r.OK() {
		t.Errorf("a database file that does not exist yet, no kubeconfig, open access and a server that is not running are all warnings: %v", r.Failures)
	}
	out := r.Render()
	for _, want := range []string{"will be created on first start", "not found", "every caller is admin", "not running"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestKindSetup(t *testing.T) {
	all := func(string) bool { return true }
	var ran []string
	run := func(out string, ok bool) Runner {
		return func(a ...string) (bool, string) { ran = append(ran, strings.Join(a, " ")); return ok, out }
	}
	if r := KindSetup("kube-sre", func(n string) bool { return n == "docker" }, run("", true)); len(r.Missing) != 2 || r.Missing[0].Name != "kind" || len(ran) != 0 {
		t.Errorf("every missing tool is reported at once and nothing is run: %+v %v", r, ran)
	}
	if r := KindSetup("kube-sre", all, run("kind other", true)); !r.Created || len(ran) != 2 || ran[1] != "kind create cluster --name kube-sre --wait 120s" || !strings.Contains(r.Detail, "kind-kube-sre") {
		t.Errorf("%+v %v", r, ran)
	}
	ran = nil
	if r := KindSetup("kube-sre", all, run("kube-sre", true)); r.Created || len(ran) != 1 || !strings.Contains(r.Detail, "already exists") {
		t.Errorf("an existing cluster is left alone: %+v %v", r, ran)
	}
	if r := KindSetup("kube-sre", all, run("Cannot connect to the Docker daemon", false)); r.Created || !strings.Contains(r.Detail, "is Docker running?") {
		t.Errorf("%+v", r)
	}
	n := 0
	failCreate := func(a ...string) (bool, string) {
		n++
		return n == 1, "boom"
	}
	if r := KindSetup("kube-sre", all, failCreate); r.Created || !strings.Contains(r.Detail, "kind create cluster failed: boom") {
		t.Errorf("%+v", r)
	}
}
