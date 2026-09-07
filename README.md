# kube-sre

An AI assistant for Kubernetes operations. It reads the live state of a cluster,
answers questions, finds root causes, and proposes fixes. Every write waits for a
human to approve it.

## What it does

- Chat with a cluster over an OpenAI style streaming API.
- Investigates on its own: a coordinator answers from a live snapshot, digs into one
  resource, or fans out to four specialists (pods, metrics, logs, events).
- Tools: `kubectl`, read only `helm`, Prometheus and Loki.
- 27 built in playbooks, 20 of them with compiled detectors that need no model calls.
- Every action is recorded in a tamper evident, hash chained decision log.
- Remembers: past incidents and their fixes, operator preferences, and a temporal graph of what
  runs where and what changed in the last 15 minutes. When any of it cannot be read, the model is
  told so, so a failed read never looks like an empty one.

## Safety

- No shell. Commands are parsed and run directly; only `| grep` is emulated.
- Writes need approval. Namespace deletes, node drains and image or resource changes ask
  even in auto approve mode.
- Roles: `superadmin`, `admin`, `operator`, `readonly`. A read only key cannot write, however
  the command is spelled.
- Secrets and service accounts are never readable. Infrastructure namespaces are blocked, and
  filtered out of listings, logs, metrics and Helm output, with a note saying the list is short.
- Connection and identity flags (`--server`, `--as`, `--kubeconfig`) are refused.

## Run it

```
export OPENAI_API_KEY=sk-...
kube-sre serve            # http://0.0.0.0:8000
```

It uses SQLite at `~/.kube-sre/kube-sre.db` unless Postgres is configured
(`DATABASE_URL`, or `POSTGRES_HOST` and friends). `kube-sre db-init` creates the schema
explicitly; `serve` also applies it on start.

Talk to a running server from the terminal:

```
kube-sre chat                      # interactive; approvals are asked for on the spot
kube-sre chat -q "why is web crashing?"
kube-sre status                    # health of the recorder, sensorium, audit and memory
kube-sre replay EPISODE_ID         # exit 0 intact, 3 broken, 4 could not be verified
kube-sre digest --hours 12         # findings, autonomous work and rollback points since then
kube-sre postmortem EPISODE_ID     # timeline citing event numbers, same exit codes as replay
```

They use `--server` / `KUBESRE_URL` (default `http://localhost:8000`) and `--key` / `KUBESRE_API_KEY`.

Settings come from environment variables, then `./.env`, then `~/.kube-sre/.env`.

| Setting | Meaning |
| --- | --- |
| `LLM_PROVIDER` | `openai` (default), `azure`, `qwen`, or `anthropic` |
| `OPENAI_API_KEY`, `OPENAI_BASE_URL` | key and optional compatible endpoint |
| `AZURE_OPENAI_*` | Azure endpoint, key, deployments |
| `KUBESRE_ADMIN_KEYS` and `_OPERATOR_`, `_READONLY_`, `_SUPERADMIN_` | comma separated API keys |
| `REQUIRE_AUTH` | refuse to start with no keys configured |
| `PROMETHEUS_URL`, `LOKI_URL` | metric and log sources |
| `KUBECTL_BLOCKED_NAMESPACES` | namespaces the agent never touches |
| `POSTMORTEM_ENABLED`, `POSTMORTEM_LLM_NARRATIVE` | postmortems on by default; optional model written prose over the timeline |
| `MEMORY_SECURITY_HARDENING` | screen user derived memory writes (rate limit, trust, injection patterns) and keep an audit chain |
| `MEMORY_BITEMPORAL_ENABLED`, `MEMORY_KG_PPR`, `MEMORY_WRITE_RECONCILE` | graph event time, blast radius ranking, write reconciliation |

With no keys configured every caller is `admin`, which is meant for local use.

## API

`POST /v1/chat/completions` streams Server Sent Events. Send `X-Session-ID` to keep a
conversation; when the reply asks for approval, answer `yes` or `no` in the same session.
Also: `GET /healthz`, `/readyz`, `/metrics`, `/v1/namespaces`, `/v1/auth/whoami`,
`/v1/events/replay/{session}` (this process only), `/v1/episodes/{id}/replay` (durable, chain
verified), `/v1/episodes/{id}/postmortem`, `/v1/digest`, `/v1/findings`, and `/v1/preferences` (GET; PUT and DELETE need `operator`).

## Tests

```
go test ./...
KUBESRE_TEST_PG_DSN='postgres://postgres:test@127.0.0.1:55432/kubesre?sslmode=disable' go test -p 1 ./...
```

The second form runs the suites against Postgres instead of SQLite.
