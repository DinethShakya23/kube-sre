# Deploying kube-sre

kube-sre runs on the shared Contabo VPS at `45.130.104.143`, next to PaperLK, Tikri and Plotter.
It is set up to touch nothing they use: no Caddy, no open ports, no firewall change, no Docker
or Kubernetes on that box. Read the server's runbook first:
`paperhub-lk/VPS-DEPLOYMENT-ALL-WEBSITES.md` (especially §9).

Moved here from `161.97.115.14` on 2026-10-27.

## How it fits together

```
visitor ── https://kubesre.3wayhandshake.nl ── Cloudflare ── tunnel (outbound only)
                                                                    │
  shared VPS 45.130.104.143, user kubesre                           │
    kube-sre-tunnel   cloudflared, connects out to Cloudflare  ◀────┘
    kube-sre          127.0.0.1:8765   /ui /v1 /admin /healthz /readyz /metrics
    kube-sre-site     127.0.0.1:9000   the marketing site (everything else)
          │
          │ Tailscale only (kubectl, Prometheus, Loki)
          ▼
  cluster VPS 161.97.115.14 (100.73.155.56 on Tailscale)
    k3d "demo" cluster: the broken shop apps, Prometheus, Loki
    kube-sre-ts-api.socket      100.73.155.56:6550 -> 127.0.0.1:6550 (Kubernetes API)
    kube-sre-ts-ingress.socket  100.73.155.56:8080 -> 127.0.0.1:8080 (Prometheus, Loki)
```

The hostname routes (which path goes to 8765 and which to 9000) live in the Cloudflare
dashboard, under the tunnel's public hostname settings. DNS points at the tunnel, not at any
server's IP, so moving servers never needs a DNS change.

## On the shared VPS

Everything belongs to the `kubesre` user. Home is `750`; secret folders are `700`, files `600`.

| Path | What |
|---|---|
| `/home/kubesre/bin/` | `kube-sre`, `kubectl` (v1.35.5, matches the cluster), `cloudflared` |
| `/home/kubesre/.kube-sre/.env` | settings and secrets: model API key, admin login hash, public demo limits |
| `/home/kubesre/.kube-sre/kube-sre.db` | SQLite: flight recorder, audit chains, memory, findings history |
| `/home/kubesre/.kube-sre/kube-sre.kubeconfig` | read only cluster credentials, pointed at `https://100.73.155.56:6550` with `tls-server-name: localhost` |
| `/home/kubesre/.cloudflared/token` | the tunnel token |
| `/home/kubesre/etc/hosts` | a private hosts file, mounted over `/etc/hosts` for kube-sre only, so `prometheus.demo.test` and `loki.demo.test` reach the cluster VPS. The server's real `/etc/hosts` is untouched. |
| `/home/kubesre/site/` | the built marketing site (`ui/site-dist`) |

Units in `/etc/systemd/system/`: `kube-sre.service`, `kube-sre-site.service`, `kube-sre-tunnel.service`.

`CLUSTER_ID=kube-sre@demo:a64129c2` is pinned in `.env`. kube-sre derives the id from the API
server address, which changed with the move to Tailscale; pinning it keeps memory (past
incidents, learned rules, the knowledge graph) attached to the same cluster.

## Isolation

Each unit runs as `kubesre` with:

- `MemoryHigh`/`MemoryMax` of 384M/512M (kube-sre), 64M/128M (site), 128M/256M (tunnel);
  `CPUWeight` 50 or 20, and `OOMScoreAdjust` 500 or 600, so under pressure kube-sre gives way
  before Tikri (+300) and long before PaperLK (−800).
- `InaccessiblePaths=/home/ubuntu /home/tikri /home/diagram /root /var/lib/postgresql`.
- `ProtectSystem=strict` and `ProtectHome=read-only`: the binaries, the home folder and the
  site are read only. Only kube-sre may write, and only to `.kube-sre/` and `.kube/`.
- `NoNewPrivileges`, an empty capability set, `PrivateTmp`, `PrivateDevices`, the kernel and
  cgroup protections, and `SystemCallFilter=@system-service`.
- Each unit is also blocked from the others' secrets: kube-sre cannot see the tunnel token,
  the site cannot see `.kube-sre/`, `.kube/` or the token, the tunnel cannot see `.kube-sre/`.

The agent's own limits still apply: no shell, `kubectl`, read only `helm`, Prometheus and Loki
only, and a cluster identity that can list, get and watch but cannot read secrets, create,
delete, patch or exec.

One limit to know about: root can read everything, and the existing sudo rules for `ubuntu`
and `tikri` run deploy scripts from inside their own home folders, so a compromise of PaperLK
or Tikri could become root. That is outside kube-sre and was left as it is. Use a model API key
that belongs to kube-sre alone, with a spend limit set at the provider.

## Deploying a new version

A push to `main` that touches `cmd/`, `internal/`, `ui/`, `go.mod` or `go.sum` deploys on its
own (`.github/workflows/deploy.yml`, or run it by hand from the Actions tab). The workflow runs
the UI checks and `go test ./...`, builds the web UI (embedded in the binary), the marketing site
and the binary, then pipes one tarball over SSH to the shared VPS.

On the server, the CD key in `/home/kubesre/.ssh/authorized_keys` is locked with
`restrict,command="/home/kubesre/deploy/deploy.sh"`: it can do nothing but hand over a release.
That script (`deploy/remote-deploy.sh` in this repo, installed root owned so kubesre can run it
but not change it) takes the shared deploy lock, installs the binary and the site with a
`.previous` copy of each, stops kube-sre so systemd restarts it (`Restart=always`, so no sudo
rule is needed), and rolls back if it is not healthy within a minute.

GitHub settings: secrets `DEPLOY_SSH_KEY` (the private key) and `DEPLOY_KNOWN_HOSTS` (the
server's pinned host key), variables `DEPLOY_HOST=45.130.104.143` and `DEPLOY_USER=kubesre`.
No GitHub deploy key is needed, because the server never pulls from GitHub.

After changing `deploy/remote-deploy.sh`, install it again by hand:

```bash
sed 's/\r$//' deploy/remote-deploy.sh | ssh root@45.130.104.143 'install -o root -g kubesre -m 750 /dev/stdin /home/kubesre/deploy/deploy.sh'
```

### By hand

Neither server has Go. Build in Docker on the cluster VPS, then stream the binary across:

```bash
# 1. build (on the cluster VPS, from the pushed commit)
ssh kube-demo 'rm -rf /tmp/ks-build && git clone -q --depth 1 https://github.com/DinethShakya23/kube-sre.git /tmp/ks-build/src &&
  docker run --rm -v /tmp/ks-build/src:/src -v ks-gomod:/go/pkg/mod -w /src -e CGO_ENABLED=0 golang:1.27 \
    sh -c "git config --global --add safe.directory /src && go build -trimpath -o /src/kube-sre ./cmd/kube-sre"'

# 2. install with a backup, under the shared deploy lock, and restart only kube-sre
ssh kube-demo 'cat /tmp/ks-build/src/kube-sre' | ssh root@45.130.104.143 'flock /var/lock/droplet-deploy.lock sh -c "
  cp -a /home/kubesre/bin/kube-sre /home/kubesre/bin/kube-sre.previous &&
  install -o kubesre -g kubesre -m 750 /dev/stdin /home/kubesre/bin/kube-sre.new &&
  mv -f /home/kubesre/bin/kube-sre.new /home/kubesre/bin/kube-sre &&
  systemctl restart kube-sre"'

# 3. check
ssh root@45.130.104.143 'curl -s 127.0.0.1:8765/healthz | head -c 200; curl -s 127.0.0.1:8765/v1/auth/whoami'
```

To roll back: `install -m 750 /home/kubesre/bin/kube-sre.previous /home/kubesre/bin/kube-sre && systemctl restart kube-sre`.

The marketing site deploys the same way: `cd ui && npm run build:site`, then copy `ui/site-dist/`
into `/home/kubesre/site/` (owner `kubesre`). It is served as files, so no restart is needed.

Restarting kube-sre clears the in memory Findings list (it refills as detectors fire). The
digest reads the database and is not affected.

## Checking the other apps after any change

```bash
ssh root@45.130.104.143 'for u in https://paperlk.app https://tikri.tech https://diagram.tikri.tech; do
  echo "$(curl -s -o /dev/null -w %{http_code} $u) $u"; done; systemctl is-active caddy paperhub tikri-api tikri-web'
```

## Moving back, or to another server

Copy `/home/kubesre/` (with an SQLite online backup of the database, never a plain copy of a
running one), recreate the three units, start the new tunnel, then stop the old one. Both
tunnels can run at once for a moment; Cloudflare splits traffic between them.

The pre-move setup on `161.97.115.14` (`kube-sre`, `cloudflared`, `kube-sre-site` units) is
disabled but still installed, as a fallback: `systemctl enable --now kube-sre kube-sre-site cloudflared`
there, then stop `kube-sre-tunnel` on the shared VPS.

## Notes

- `METRICS_ENABLED=false` in `.env`: nothing scrapes kube-sre, and the tunnel's path rule in
  Cloudflare still routes `/metrics`, which made it public. It now answers 404. Remove `metrics`
  from that rule too if you ever turn it back on.
- Use a model API key that belongs to kube-sre alone, with a spend limit at the provider. To
  replace it without it ever appearing on screen, run this in your own terminal:

  ```bash
  ssh -t root@45.130.104.143 'read -rsp "New API key: " K; echo; sed -i "s|^OPENAI_API_KEY=.*|OPENAI_API_KEY=$K|" /home/kubesre/.kube-sre/.env && systemctl restart kube-sre && echo done'
  ```
