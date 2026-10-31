# Setting up kube-sre

A step by step guide, from nothing to a running kube-sre watching your cluster. For a two minute
version, see the [quick start in the README](README.md#quick-start). To try it without
installing anything, use the [live demo](https://kubesre.3wayhandshake.nl/ui/).

```mermaid
flowchart LR
  A[1. Build the binary] --> B[2. Pick a cluster]
  B --> C["3. Configure<br/>kube-sre init"]
  C --> D["4. Start<br/>kube-sre serve"]
  D --> E["5. Open the UI<br/>and ask a question"]
  E -.-> F["Optional: metrics and logs,<br/>more users, run as a service,<br/>Postgres, a public demo"]
```

## What you need

- **Go 1.27** to build the binary.
- **`kubectl`** and a kubeconfig for the cluster you want kube-sre to watch.
- **An API key for a chat model** behind an OpenAI compatible API: OpenAI itself, Azure
  OpenAI, Qwen (DashScope), or any compatible gateway such as OpenRouter or LiteLLM.
- Optional: **Prometheus** and **Loki** for metrics and logs, **Postgres** if you want more than
  one server process and **Docker** and **kind** if you want kube-sre to create a local test
  cluster for you.

kube-sre runs on Linux, macOS and Windows. Running it as a background service with
`kube-sre service install` needs Linux with systemd.

## 1. Build the binary

```bash
git clone https://github.com/DinethShakya23/kube-sre.git
cd kube-sre
go build -o kube-sre ./cmd/kube-sre
./kube-sre version
```

The web UI is already built into the binary, so you do not need Node unless you want to change
the UI. Put `kube-sre` somewhere on your `PATH` (for example `/usr/local/bin`); the rest of this
guide assumes you did.

## 2. Pick a cluster

kube-sre talks to whatever cluster your kubeconfig points at: `~/.kube/config` by default, or
the file you set in `KUBECONFIG_PATH`.

**Have a cluster already?** Make sure `kubectl get pods -A` works for the user that will run
kube-sre. A read only identity is enough to ask questions. kube-sre can only change things if
its identity is allowed to and even then every change waits for your approval.

**No cluster? Make a local one** (needs Docker, kind and kubectl):

```bash
kube-sre kind-setup                   # creates a kind cluster called kube-sre
kubectl apply -f examples/shop.yaml   # one healthy app and four broken ones to investigate
```

The shop is the same one the live demo uses; [DEMO.md](DEMO.md) explains what is wrong with
each app.

## 3. Configure

```bash
kube-sre init
```

`init` asks for your model provider and API key, writes everything to `~/.kube-sre/.env` and
**prints an admin API key once**. Save that key somewhere safe, because the UI and the command
line client both ask for it.

You can also answer up front, for scripts:

```bash
kube-sre init --provider openai --api-key sk-... --yes
kube-sre init --provider azure --api-key ... --endpoint https://my-resource.openai.azure.com --yes
kube-sre init --postgres "postgres://user:pass@host:5432/kubesre" --yes   # Postgres instead of SQLite
```

Change a setting at any time with `kube-sre set`, which also restarts the service if it runs as
one:

```bash
kube-sre set OPENAI_COORDINATOR_MODEL=gpt-4o OPENAI_SUBAGENT_MODEL=gpt-4o-mini
kube-sre set OPENAI_BASE_URL=https://openrouter.ai/api/v1   # any OpenAI compatible endpoint
```

Settings are read from environment variables first, then `./.env`, then `~/.kube-sre/.env`. The
full list is in the [README's configuration section](README.md#configuration).

| Provider | What to set |
|---|---|
| OpenAI | `OPENAI_API_KEY`; optionally `OPENAI_COORDINATOR_MODEL` (default `gpt-4o`) and `OPENAI_SUBAGENT_MODEL` (default `gpt-4o-mini`) |
| An OpenAI compatible gateway | the same, plus `OPENAI_BASE_URL` and the gateway's model names |
| Azure OpenAI | `LLM_PROVIDER=azure` and the `AZURE_OPENAI_*` endpoint, key and deployment settings |
| Qwen (DashScope) | `LLM_PROVIDER=qwen` and `DASHSCOPE_API_KEY`; the endpoint and models are filled in |

## 4. Start it

```bash
kube-sre serve --host 127.0.0.1 --port 8000
```

`--host 127.0.0.1` keeps it reachable from this machine only, which is the safe default while
you try it out. On start it creates the database schema, connects to the cluster and starts
watching pods and events.

In another terminal, check that everything is healthy:

```bash
kube-sre status   # a local dashboard; exits non zero if something is broken
kube-sre health   # what the running server reports about itself
```

## 5. Open the UI and ask a question

Open http://localhost:8000/ and paste the admin key from step 3. Then ask, for example,
*"Is anything broken right now?"* or, with the example shop, *"Why is checkout crashing?"*

Or use the terminal:

```bash
export KUBESRE_API_KEY=<the admin key>
kube-sre chat                              # interactive; approvals are asked for on the spot
kube-sre chat -q "why is checkout crashing?"
```

Within a few minutes the **Findings** page fills up with what the built in detectors noticed,
and the **Digest** summarises the day.

**Running kube-sre on a remote machine?** Leave it on `127.0.0.1` and tunnel to it from your
laptop: `ssh -L 8000:127.0.0.1:8000 user@that-machine`, then open http://localhost:8000/.

---

## Optional next steps

### Add metrics and logs

kube-sre answers a lot from `kubectl` alone, but with Prometheus and Loki it can look at memory
and restart history and search logs across pods and time:

```bash
kube-sre set PROMETHEUS_URL=http://prometheus.example:9090 LOKI_URL=http://loki.example:3100
```

Don't have them? [DEMO.md](DEMO.md#build-your-own) installs both, with Alloy to collect logs,
using the same Helm values as the live demo.

### Add more people

Each person or tool gets its own API key, by role:

| Role | Can do |
|---|---|
| `readonly` | ask questions and read everything kube-sre can see; can never change anything |
| `operator` | also propose and (with approval) run everyday changes: scale, restart, apply |
| `admin` | also higher risk changes such as deletes and drains (always with approval) and managing detectors |
| `superadmin` | everything, including protected namespaces |

```bash
kube-sre set KUBESRE_READONLY_KEYS=key-for-alice,key-for-the-dashboard
kube-sre set KUBESRE_OPERATOR_KEYS=key-for-bob
kube-sre set REQUIRE_AUTH=true   # refuse to start if no keys are configured
```

Use long random strings for keys, for example `openssl rand -hex 24`.

### Run it as a service (Linux)

```bash
kube-sre service install   # a systemd user unit that starts now and on boot
kube-sre service status
sudo loginctl enable-linger "$USER"   # keep it running after you log out
```

`kube-sre service uninstall` removes it. On macOS or Windows, run `kube-sre serve` under your own
supervisor (launchd, a scheduled task, or a container).

### Use Postgres

SQLite is fine for one server. Use Postgres when you want several kube-sre processes behind a
load balancer; leader election then makes sure only one of them runs the background work.

```bash
kube-sre set DATABASE_URL=postgres://user:pass@host:5432/kubesre
kube-sre db-init   # create or update the schema now (serve also does it on start)
```

Take a backup manifest beside your database backups and check it after a restore:
`kube-sre backup-manifest -o manifest.json`, later `kube-sre backup-verify manifest.json`.

### Put it on the internet as a public demo

Only do this deliberately. In public demo mode, visitors without a key get a read only role
with limits and admins sign in with a password:

```bash
kube-sre admin-hash   # asks for a password (12 characters or more) and prints its hash
kube-sre set PUBLIC_DEMO=true ADMIN_USERNAME=admin ADMIN_PASSWORD_HASH='<the printed hash>'
kube-sre set PUBLIC_DEMO_QUESTIONS_PER_HOUR=20 PUBLIC_DEMO_DAILY_BUDGET_USD=2
```

Optionally add a Cloudflare Turnstile check before a visitor's first question
(`TURNSTILE_SITE_KEY`, `TURNSTILE_SECRET_KEY`). Point the read only demo at a cluster that only
holds demo workloads, give kube-sre a read only identity there and use a model key with a spend
limit at the provider. [DEMO.md](DEMO.md) shows how the live demo does all of this, including
serving it through a Cloudflare Tunnel so the server opens no ports.

## When something goes wrong

| What you see | What to check |
|---|---|
| `kube-sre status` shows the cluster as unreachable | `kubectl get pods -A` as the same user and `KUBECONFIG_PATH` |
| Answers say the model call failed | the API key, `OPENAI_BASE_URL` and the model names your provider expects |
| "This server needs an API key" in the UI | paste a key from `init` or from the `KUBESRE_*_KEYS` settings |
| The Findings page says nothing is watching | the `sensorium` block in `kube-sre health`; the cluster may be unreachable, or `SENSORIUM_ENABLED=false` |
| Metrics or log questions say "not configured" | set `PROMETHEUS_URL` and `LOKI_URL` |
| The digest says it has gaps | it lists exactly which source could not be read |

The server logs every request and decision; with the service, read them with
`journalctl --user -u kube-sre -f`.
