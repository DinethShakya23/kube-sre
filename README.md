# kube-sre

**An AI SRE for Kubernetes.**

Ask it what's wrong with your cluster in plain English. It reads the live cluster, works out the
root cause, shows you the evidence it used, and suggests a fix. It never changes anything until a
person says yes.

🌐 **Website:** https://kubesre.3wayhandshake.nl
▶️ **Live demo:** https://kubesre.3wayhandshake.nl/ui/

Open source, self hosted, and read only by default. One Go binary with the web UI built in.

[Set it up](SETUP.md) · [How it works inside](ARCHITECTURE.md) · [The demo: cluster, hosting and deploys](DEMO.md)

---

## Try it in your browser

The [live demo](https://kubesre.3wayhandshake.nl/ui/) watches a small sandbox cluster with a few
apps broken on purpose. No sign up, nothing to install. Try asking:

- *Why is checkout crashing?*
- *Why can't payments start?*
- *What is wrong with recommender?*

It is read only, so nothing you ask can change the cluster. Each visitor gets 20 questions an
hour, and the whole demo shares a small daily budget. [DEMO.md](DEMO.md) explains what is in the
demo cluster and how to build your own.

## What it does

- **Answers questions about your cluster.** It reads pods, events, logs, rollouts and
  deployments with `kubectl`, and metrics and logs from Prometheus and Loki when you have them.
  Bigger questions get a plan and four specialists (pods, metrics, logs, events) instead of one
  guess.
- **Notices problems on its own.** 27 built in playbooks, 20 of them with detectors that run
  without calling the model at all, so spotting a crash loop costs nothing. You can describe a new
  failure in plain English and it becomes a detector that watches quietly until you promote it.
- **Keeps a record you can check.** Every step goes into a tamper evident, hash chained log. You
  can replay any investigation, read a postmortem built only from what was recorded, and get a
  digest of what happened while you were away.
- **Remembers.** Past incidents and what fixed them, your preferences, and a map of what runs
  where and what changed recently. If any of that can't be read, it says so, so a failed read
  never looks like an empty one.

## How it keeps your cluster safe

- **Nothing changes without you.** Every write is shown to you with a risk level first. Deleting
  a namespace, draining a node, or changing an image or resources always asks, even when
  approvals are set to automatic.
- **No shell.** Commands are parsed and run directly. Only `| grep` is emulated.
- **Roles.** `superadmin`, `admin`, `operator` and `readonly`. A read only key cannot write,
  however the command is phrased.
- **Off limits.** Secrets and service accounts are never readable. Infrastructure namespaces are
  blocked and filtered out of listings, logs, metrics and Helm output, with a note saying the
  list was shortened. Connection and identity flags (`--server`, `--as`, `--kubeconfig`) are
  refused.

## Architecture

```mermaid
flowchart LR
  U(["You<br/>web UI · CLI · API"]) --> API["kube-sre API<br/>auth, roles, limits"]
  API --> AG["Agent<br/>coordinator + 4 specialists"]
  AG --> T["Guarded tools<br/>kubectl · helm · Prometheus · Loki"]
  T --> K[(Your cluster)]
  K -->|watch| D["Detectors<br/>no model call"]
  D --> AG
  AG <--> M["Memory<br/>incidents, rules, preferences"]
  AG --> R["Hash chained record<br/>digest · postmortems"]
  AG -.->|"every change waits"| OK{Your approval}
```

You ask through the web UI, the CLI or the API. The agent reads the live cluster through guarded
tools, uses what it remembers from past incidents, and records every step in a tamper evident
log. Detectors watch the cluster on their own without calling the model. Any change it proposes
waits for a person to approve it. [ARCHITECTURE.md](ARCHITECTURE.md) goes deeper: the path of a
question, the safety layers, detection, memory and the record.

## Quick start

You need Go 1.27 to build, `kubectl`, a cluster, and an API key for any OpenAI compatible model.
For the full walkthrough, with providers, more users, running as a service and troubleshooting,
see [SETUP.md](SETUP.md).

```bash
go build -o kube-sre ./cmd/kube-sre

./kube-sre kind-setup                 # a local cluster, if you don't have one
kubectl apply -f examples/shop.yaml   # one healthy app, four broken ones to investigate
export OPENAI_API_KEY=sk-...
./kube-sre init                       # writes ~/.kube-sre/.env and prints an admin key once
./kube-sre serve                      # then open http://localhost:8000/
```

Paste the admin key into the page when it asks. `kube-sre status` is a local health dashboard
(it exits non zero when something is broken), `kube-sre set KEY=VALUE` changes a setting, and
`kube-sre service install` runs it as a systemd user service.

Data lives in SQLite at `~/.kube-sre/kube-sre.db`, or Postgres if you set `DATABASE_URL` (or
`POSTGRES_HOST` and friends). `serve` creates the schema on start; `kube-sre db-init` does it
on its own.

## Using it

**In the browser.** The web UI is at `/ui/` (the root redirects there). It has chat with
Approve and Deny buttons, findings with search and filters, the digest, postmortems with the
record's verdict, status, and detector and preference management. On a remote host, tunnel
first: `ssh -L 8765:127.0.0.1:8765 user@host`.

**From the terminal**, against a running server:

```bash
kube-sre chat                        # interactive; asks you to approve changes on the spot
kube-sre chat -q "why is web crashing?"
kube-sre digest --hours 12           # what fired, what it did, and how to undo it
kube-sre postmortem EPISODE_ID       # a timeline that cites recorded events
kube-sre replay EPISODE_ID           # the raw record; exit 0 intact, 3 broken, 4 unverified
kube-sre health                      # the running server's subsystems
kube-sre detector new "pods killed for running out of memory"   # starts in shadow mode
kube-sre detector shadow NAME        # what a shadow detector would have fired
kube-sre detector promote NAME       # a human decision; refused if it can never fire
```

The terminal commands use `--server` / `KUBESRE_URL` (default `http://localhost:8000`) and
`--key` / `KUBESRE_API_KEY`.

## Configuration

Settings come from environment variables first, then `./.env`, then `~/.kube-sre/.env`.

### The essentials

| Setting | What it does |
| --- | --- |
| `LLM_PROVIDER` | `openai` (default, any OpenAI compatible endpoint), `azure` or `qwen`. `anthropic` is accepted but has no native backend yet, so it uses the OpenAI compatible endpoint |
| `OPENAI_API_KEY`, `OPENAI_BASE_URL` | the model key, and an optional compatible endpoint |
| `AZURE_OPENAI_*` | Azure endpoint, key and deployments |
| `KUBESRE_ADMIN_KEYS`, `KUBESRE_OPERATOR_KEYS`, `KUBESRE_READONLY_KEYS`, `KUBESRE_SUPERADMIN_KEYS` | comma separated API keys per role |
| `REQUIRE_AUTH` | refuse to start with no keys configured |
| `PROMETHEUS_URL`, `LOKI_URL` | where to find metrics and logs |
| `KUBECTL_BLOCKED_NAMESPACES` | namespaces it never touches |

With no keys configured every caller is `admin`, which is meant for local use only.

### Running a public demo

| Setting | What it does |
| --- | --- |
| `PUBLIC_DEMO` | visitors with no key get a read only `public` role: chat, findings, digest and postmortems, never a write. A public demo is never open admin |
| `PUBLIC_DEMO_QUESTIONS_PER_HOUR`, `PUBLIC_DEMO_MAX_QUESTION_CHARS`, `PUBLIC_DEMO_MAX_CONCURRENT` | per visitor and overall limits (defaults 20, 500, 4) |
| `PUBLIC_DEMO_DAILY_BUDGET_USD` | model spend shared by all visitors, reset at 00:00 UTC (default 5) |
| `TURNSTILE_SITE_KEY`, `TURNSTILE_SECRET_KEY` | a Cloudflare Turnstile check before a visitor's first question |
| `ADMIN_USERNAME`, `ADMIN_PASSWORD_HASH` | password sign in at `/admin`; make the hash with `kube-sre admin-hash` |
| `ADMIN_SESSION_HOURS`, `ADMIN_LOGIN_MAX_FAILS` | how long a sign in lasts (8 hours), and failures before a 15 minute lockout (5) |

### Detectors, memory and reports

| Setting | What it does |
| --- | --- |
| `NL_DETECTOR_AUTHORING_ENABLED`, `DB_DETECTOR_REFRESH_SECONDS` | plain English detectors: staged in shadow mode, promoted by a person, reloaded without a restart |
| `POSTMORTEM_ENABLED`, `POSTMORTEM_LLM_NARRATIVE` | postmortems (on by default), with optional model written prose over the timeline |
| `MEMORY_SECURITY_HARDENING` | screen memory writes that come from users (rate limit, trust, injection patterns) and keep an audit chain |
| `MEMORY_PROMOTION`, `MEMORY_SUMMARY_TREE`, `MEMORY_SUMMARY_MIN_CLUSTER` | learn IF-THEN rules from verified recurring fixes, and per theme summaries rebuilt only when a theme changes |
| `MEMORY_PROSPECTIVE`, `MEMORY_RETENTION_DAYS` | re-check after a fix by actually reading the cluster again, and prune telemetry (never the chained ledgers or episodes) |
| `LEADER_ELECTION_ENABLED`, `LEADER_ELECTION_POLL_SECONDS` | on Postgres, only one replica runs the background work; the rest serve the API |

<details>
<summary><b>Experimental features</b></summary>

| Setting | What it does |
| --- | --- |
| `CORTEX_V5_ENABLED` + `KI_V5_CHANGE_LEDGER`, `KI_V5_CHANGE_FIRST_RCA`, `KI_V5_INVESTIGATION_WRITEBACK` | rank recent changes first in an investigation, and feed evidence back to the graph |
| `KI_V5_FILE_PLANE`, `KI_V5_FILE_PLANE_DIR`, `KI_V5_FILE_PLANE_MAX_BYTES` | with `CORTEX_V5_ENABLED`, keep bounded CLUSTER.md and MEMORY.md summaries of what the agent knows |
| `CORTEX_V5_ENABLED` + `KI_V5_RUNBOOK_SKILLS`, `KI_V5_HARNESS_FANOUT`, `KI_V5_VERIFY_LADDER`, `KI_V5_ESCALATION_BRIEFS`, `KI_V5_RESPONSIVENESS` | runbooks as skills, read only investigators, an adversarial review note, a responder brief with escalation conditions, and progress heartbeats |
| `KI_V5_CHANGE_WATCHDOG`, `KI_V5_PREDICTIVE_FUSION`, `KI_V5_PREDICTIVE_PRECAPTURE` | a read only look at what a change did and at a predicted failure, and recorders armed before a predicted crash |
| `MEMORY_BITEMPORAL_ENABLED`, `MEMORY_KG_PPR`, `MEMORY_WRITE_RECONCILE` | graph event time, blast radius ranking, and write reconciliation |

`GET /v1/v5/status` (and `/healthz`) show which experimental features are on, which settings you
changed that nothing reads, and the state of the write brakes: `KI_V5_KILL_SWITCH`,
`KI_V5_CHANGE_FREEZE`, and automatic revocation of autonomous fixes
(`KI_V5_STATISTICAL_PROMOTION`, revoke only).

</details>

## Keeping the record safe

The decision log and audit trails are hash chained, so tampering shows. These commands talk to
the database directly:

```bash
kube-sre chain-export memory_audit CLUSTER_ID -o audit.json   # a self verifying archive
kube-sre chain-verify audit.json                              # needs no database
kube-sre chain-truncate audit.json --note "quarterly"         # removes exactly those rows, after declaring the gap
kube-sre backup-manifest -o manifest.json                     # take it beside your pg_dump
kube-sre backup-verify manifest.json                          # after a restore: did everything come back?
```

Retention never prunes the chains. A restore that loses the newest rows of a chain breaks no
link, so only the manifest, which records how far each chain got, can tell. `kube-sre provenance`
explains how to verify a release.

<details>
<summary><b>API</b></summary>

`POST /v1/chat/completions` streams Server Sent Events. Send `X-Session-ID` to keep a
conversation going; when a reply asks for approval, answer `yes` or `no` in the same session.

Also: `GET /healthz`, `/readyz`, `/metrics`, `/v1/namespaces`, `/v1/auth/whoami`,
`/v1/events/replay/{session}` (this process only), `/v1/episodes/{id}/replay` (durable, chain
verified), `/v1/episodes/{id}/postmortem`, `/v1/digest`, `/v1/detectors` (author, list, promote,
demote, shadow findings), `/v1/findings`, and `/v1/preferences` (GET; PUT and DELETE need
`operator`).

Without a key: `GET /v1/public/config`, `POST /v1/public/verify` (Turnstile), and
`POST /v1/admin/login` and `/v1/admin/logout`. Those two need the `X-KubeSre-CSRF: 1` header, as
does any write made with the sign in cookie.

</details>

## Project structure

For how these pieces fit together (a question's path, the safety layers, detection, memory and
the record), see [ARCHITECTURE.md](ARCHITECTURE.md).

```
cmd/kube-sre/        the single binary: server, CLI client and admin commands
internal/
  agent/             the coordinator, the four specialists, approvals and conversation state
  kube/ helm/        kubectl and helm, parsed and guarded (no shell, risk tiers, blocked namespaces)
  prom/ loki/        metrics and log queries
  playbooks/         the 27 failure playbooks (YAML)
  detect/ sensorium/ perception/   watching the cluster and the zero cost detectors
  memory/            incidents, preferences, learned rules and the knowledge graph
  recorder/ audit/ chainops/       the hash chained record, export and verification
  digest/            the digest and postmortems
  autonomy/ cortex/  trust levels, brakes and the experimental investigation features
  api/ web/          the HTTP API, auth and public demo mode, and the embedded web UI
  store/ schema/     SQLite or Postgres, and migrations
ui/                  the React web UI (built into internal/web/static) and ui/site, the website
examples/            shop.yaml, a cluster with broken apps to investigate
deploy/              the server side of continuous deployment
.github/workflows/   test, build and deploy on push to main
```

## Development

**The web UI** is a React app in `ui/`, using components from [React Bits](https://reactbits.dev).
Its build is committed to `internal/web/static`, so `go build` never needs Node.

```bash
cd ui && npm install
npm run dev        # http://localhost:5173/ui/, talking to the server at 127.0.0.1:8765 (or $KUBESRE_URL)
npm run build      # writes internal/web/static; commit the result
npm run dev:site   # the website in ui/site
npm run build:site # writes ui/site-dist, a static folder for the domain root (not committed)
```

**Tests:**

```bash
go test ./...
KUBESRE_TEST_PG_DSN='postgres://postgres:test@127.0.0.1:55432/kubesre?sslmode=disable' go test -p 1 ./...
```

The second form runs the same suites against Postgres instead of SQLite.

**Deploying:** [DEMO.md](DEMO.md) describes how the public demo runs, and how pushes to
`main` deploy it.
