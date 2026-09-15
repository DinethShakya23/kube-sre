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

First time: `kube-sre init` writes `~/.kube-sre/.env` (and prints an admin key once); `kube-sre set KEY=VALUE`
changes a setting; `kube-sre status` is a local dashboard that exits non zero when something is broken;
`kube-sre service install` runs it as a systemd user service; `kube-sre kind-setup` makes a local cluster;
`kube-sre provenance` says how to verify a release.

Talk to a running server from the terminal:

```
kube-sre chat                      # interactive; approvals are asked for on the spot
kube-sre chat -q "why is web crashing?"
kube-sre health                    # the running server's recorder, sensorium, audit, memory and leader blocks
kube-sre replay EPISODE_ID         # exit 0 intact, 3 broken, 4 could not be verified
kube-sre digest --hours 12         # findings, autonomous work and rollback points since then
kube-sre postmortem EPISODE_ID     # timeline citing event numbers, same exit codes as replay
kube-sre detector new "pods killed for memory"   # authored in plain English, starts as a shadow
kube-sre detector shadow NAME      # what a shadow detector fired, and whether it was evaluated at all
kube-sre detector promote NAME     # a human decision; refused if the detector can never fire
```

Or open the web UI at `http://HOST:PORT/` (it is inside the binary, no build step). It has chat
with Approve and Deny buttons, findings, the digest, postmortems with the chain verdict, status,
and detector and preference management. Paste an API key on first use; the page itself is static
and every data call still needs the key. On a remote host, tunnel first:
`ssh -L 8765:127.0.0.1:8765 user@host`.

They use `--server` / `KUBESRE_URL` (default `http://localhost:8000`) and `--key` / `KUBESRE_API_KEY`.

Operating the hash chains and backups (these talk to the database directly):

```
kube-sre chain-export memory_audit CLUSTER_ID -o audit.json   # a self-verifying archive
kube-sre chain-verify audit.json                              # needs no database
kube-sre chain-truncate audit.json --note "quarterly"         # removes exactly those rows, after declaring the gap
kube-sre backup-manifest -o manifest.json                     # take beside your pg_dump
kube-sre backup-verify manifest.json                          # after a restore: did everything come back?
```

Retention never prunes the chains. A restore that drops the newest rows of a chain breaks no
link, so only the manifest, which records how far each chain got, can tell.

`GET /v1/v5/status` (and `/healthz`) report which experimental slices are on, which settings you
changed that no code reads, which are on inside a subsystem that is not running, and the state of the
write brakes: `KI_V5_KILL_SWITCH`, `KI_V5_CHANGE_FREEZE`, and the statistical revocation of autonomous
fixes (`KI_V5_STATISTICAL_PROMOTION`, revoke only).

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
| `NL_DETECTOR_AUTHORING_ENABLED`, `DB_DETECTOR_REFRESH_SECONDS` | plain English detectors: staged as shadow, promoted by a human, reloaded without a restart |
| `POSTMORTEM_ENABLED`, `POSTMORTEM_LLM_NARRATIVE` | postmortems on by default; optional model written prose over the timeline |
| `LEADER_ELECTION_ENABLED`, `LEADER_ELECTION_POLL_SECONDS` | on Postgres only one replica runs the sensorium, watchtower and consolidation; the rest serve the API |
| `MEMORY_SECURITY_HARDENING` | screen user derived memory writes (rate limit, trust, injection patterns) and keep an audit chain |
| `MEMORY_PROMOTION`, `MEMORY_SUMMARY_TREE`, `MEMORY_SUMMARY_MIN_CLUSTER` | learned IF-THEN rules from verified recurring fixes, and per-theme summaries rebuilt only when a theme changes |
| `MEMORY_PROSPECTIVE`, `MEMORY_RETENTION_DAYS` | post-fix re-checks that actually re-read the cluster; and pruning of telemetry (never the chained ledgers or episodes) |
| `CORTEX_V5_ENABLED` + `KI_V5_CHANGE_LEDGER`, `KI_V5_CHANGE_FIRST_RCA`, `KI_V5_INVESTIGATION_WRITEBACK` | rank recent changes first in an investigation, and feed evidence back to the graph |
| `KI_V5_FILE_PLANE`, `KI_V5_FILE_PLANE_DIR`, `KI_V5_FILE_PLANE_MAX_BYTES` | with `CORTEX_V5_ENABLED`, regenerate bounded CLUSTER.md and MEMORY.md projections of what the agent knows |
| `CORTEX_V5_ENABLED` + `KI_V5_RUNBOOK_SKILLS`, `KI_V5_HARNESS_FANOUT`, `KI_V5_VERIFY_LADDER`, `KI_V5_ESCALATION_BRIEFS`, `KI_V5_RESPONSIVENESS` | matched runbooks as skills; read-only investigators over the ACI verbs; an adversarial review note; a responder brief with explicit escalation conditions; progress heartbeats and latency budgets |
| `KI_V5_CHANGE_WATCHDOG`, `KI_V5_PREDICTIVE_FUSION`, `KI_V5_PREDICTIVE_PRECAPTURE` | a read-only look at what a change did, at a predicted failure, and recorders armed before a predicted death |
| `MEMORY_BITEMPORAL_ENABLED`, `MEMORY_KG_PPR`, `MEMORY_WRITE_RECONCILE` | graph event time, blast radius ranking, write reconciliation |

With no keys configured every caller is `admin`, which is meant for local use.

## API

The UI is served from `/ui/` and `/` redirects there.

`POST /v1/chat/completions` streams Server Sent Events. Send `X-Session-ID` to keep a
conversation; when the reply asks for approval, answer `yes` or `no` in the same session.
Also: `GET /healthz`, `/readyz`, `/metrics`, `/v1/namespaces`, `/v1/auth/whoami`,
`/v1/events/replay/{session}` (this process only), `/v1/episodes/{id}/replay` (durable, chain
verified), `/v1/episodes/{id}/postmortem`, `/v1/digest`, `/v1/detectors` (author, list, promote, demote, shadow findings), `/v1/findings`, and `/v1/preferences` (GET; PUT and DELETE need `operator`).

## Tests

```
go test ./...
KUBESRE_TEST_PG_DSN='postgres://postgres:test@127.0.0.1:55432/kubesre?sslmode=disable' go test -p 1 ./...
```

The second form runs the suites against Postgres instead of SQLite.
