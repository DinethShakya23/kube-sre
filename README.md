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

With no keys configured every caller is `admin`, which is meant for local use.

## API

`POST /v1/chat/completions` streams Server Sent Events. Send `X-Session-ID` to keep a
conversation; when the reply asks for approval, answer `yes` or `no` in the same session.
Also: `GET /healthz`, `/readyz`, `/metrics`, `/v1/namespaces`, `/v1/auth/whoami`,
`/v1/events/replay/{session}`.

## Tests

```
go test ./...
KUBESRE_TEST_PG_DSN='postgres://postgres:test@127.0.0.1:55432/kubesre?sslmode=disable' go test -p 1 ./...
```

The second form runs the suites against Postgres instead of SQLite.
