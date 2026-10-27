# Deploying kube-sre

kube-sre and the demo cluster it watches both run on the shared Contabo VPS at `45.130.104.143`,
next to PaperLK, Tikri and Plotter. They are set up to stay out of their way: no Caddy block, no
open ports, no firewall rule changes, no Docker, and a rootless cluster in its own network
namespace. Read the server's runbook first: `paperhub-lk/VPS-DEPLOYMENT-ALL-WEBSITES.md` (§9).

Moved here from `161.97.115.14` on 2026-10-27; the demo cluster followed the same day.

## How it fits together

```
visitor ── https://kubesre.3wayhandshake.nl ── Cloudflare ── tunnel (outbound only)
                                                                    │
  shared VPS 45.130.104.143                                         │
   user kubesre                                                     │
    kube-sre-tunnel   cloudflared, connects out to Cloudflare  ◀────┘
    kube-sre          127.0.0.1:8765   /ui /v1 /admin /healthz /readyz
    kube-sre-site     127.0.0.1:9000   the marketing site (everything else)
          │ kubectl to 127.0.0.1:6443, Prometheus 127.0.0.1:30090, Loki 127.0.0.1:30100
          ▼
   user kubesre-cluster (rootless k3s, its own user and network namespace)
    kubesre-cluster                 k3s v1.35.5+k3s1: the broken shop apps, Prometheus, Loki, Alloy
    kubesre-cluster-pf-prometheus   kubectl port-forward on 127.0.0.1:30090 only
    kubesre-cluster-pf-loki         kubectl port-forward on 127.0.0.1:30100 only
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
| `/home/kubesre/.kube-sre/kube-sre.kubeconfig` | read only credentials (the `sre-agent/kube-sre` ServiceAccount token) for `https://127.0.0.1:6443` |
| `/home/kubesre/.cloudflared/token` | the tunnel token |
| `/home/kubesre/site/` | the built marketing site (`ui/site-dist`) |

`.env` points kube-sre at `PROMETHEUS_URL=http://127.0.0.1:30090` and `LOKI_URL=http://127.0.0.1:30100`.

Units in `/etc/systemd/system/`: `kube-sre.service`, `kube-sre-site.service`, `kube-sre-tunnel.service`.

`CLUSTER_ID=kube-sre@demo:a64129c2` is pinned in `.env`. kube-sre derives the id from the API
server address, which changed in each move; pinning it keeps memory (past incidents, learned
rules, the knowledge graph) attached to the same demo cluster.

## The demo cluster

Rootless k3s, owned by the `kubesre-cluster` user (home `750`, no login shell). It runs as that
user, never as root, inside its own user and network namespace, so it adds no network
interfaces and no firewall rules to the server.

| Path | What |
|---|---|
| `/home/kubesre-cluster/bin/` | `k3s` v1.35.5+k3s1 (with `kubectl`, `crictl`, `ctr` links) and `helm` v3.19.0 |
| `/home/kubesre-cluster/.config/k3s/config.yaml` | Traefik and servicelb disabled, so nothing binds 80/443 |
| `/home/kubesre-cluster/.kube/k3s.yaml` | the admin kubeconfig (stays with this user) |
| `/home/kubesre-cluster/demo/` | `shop.yaml`, `rbac.yaml` and the Prometheus, Loki and Alloy values |

What runs in it: the `shop` apps (one healthy, four broken on purpose, with a quota), Prometheus
29.31.1 (no node-exporter, which needs host access), Loki 7.3.0 and Alloy 1.12.1 in `monitoring`,
and the `sre-agent/kube-sre` ServiceAccount bound to `view` plus `kube-sre-extra-read`. It can
list, get and watch, and cannot read secrets, create, delete, patch or exec.

To work on it, as root on the server:

```bash
K="sudo -u kubesre-cluster -H env KUBECONFIG=/home/kubesre-cluster/.kube/k3s.yaml PATH=/home/kubesre-cluster/bin:/usr/bin:/bin"
$K kubectl get pods -A
$K kubectl apply -f /home/kubesre-cluster/demo/shop.yaml
$K helm list -A
```

### What it needed from the server

These are the only server wide changes, each small and easy to undo:

| Change | Why | Undo |
|---|---|---|
| packages `uidmap`, `slirp4netns` | user namespaces and the cluster's private network | `apt-get remove uidmap slirp4netns` |
| `/etc/apparmor.d/kubesre-cluster` | lets only binaries under `/home/kubesre-cluster/{bin,.rancher}` create user namespaces; Ubuntu blocks that for everything else | `apparmor_parser -R kubesre-cluster && rm /etc/apparmor.d/kubesre-cluster` |
| `/etc/sysctl.d/90-kubesre-cluster.conf` | `net.ipv4.ip_forward = 1`; k3s refuses to start without it. ufw keeps routed traffic denied, so nothing is forwarded | remove the file, `sysctl -w net.ipv4.ip_forward=0` |

### Its limits

`kubesre-cluster.service` holds the whole cluster, every pod included (`Delegate=yes`):
`MemoryHigh=2560M`, `MemoryMax=3G`, `CPUQuota=150%` (1.5 of 4 cores), `CPUWeight=30`, `IOWeight=30`,
`OOMScoreAdjust=600`, and the other apps' homes, `/home/kubesre` and `/root` made inaccessible.
It sits near its soft memory limit, mostly cached image layers; PaperLK and Tikri stay
unaffected. The k3s API on port 6443 listens on the host but ufw blocks it from the internet.

The two `kubesre-cluster-pf-*` units bind `127.0.0.1` only and restart on their own if a pod
moves. They are part of `kubesre-cluster.service`, so stopping it stops them too.

To remove the cluster completely: stop and disable the three `kubesre-cluster*` units, delete
their unit files, `userdel -r kubesre-cluster`, then undo the three changes above.

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

Prefer re-running the workflow from the Actions tab. The server has neither Go nor Docker, and
should not get them, so a manual build happens on any machine with Go 1.27, then streams across:

```bash
# 1. build (anywhere with Go, from the pushed commit)
CGO_ENABLED=0 go build -trimpath -o kube-sre ./cmd/kube-sre

# 2. install with a backup, under the shared deploy lock, and restart only kube-sre
cat kube-sre | ssh root@45.130.104.143 'flock /var/lock/droplet-deploy.lock sh -c "
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
  echo "$(curl -s -o /dev/null -w "%{http_code} %{time_total}s" $u) $u"; done
  systemctl is-active caddy paperhub tikri-api tikri-web kubesre-cluster kube-sre
  echo "cluster memory: $(( $(cat /sys/fs/cgroup/system.slice/kubesre-cluster.service/memory.current)/1048576 ))M"'
```

## Moving to another server

Copy `/home/kubesre/` (with an SQLite online backup of the database, never a plain copy of a
running one), recreate the three units, start the new tunnel, then stop the old one. Both
tunnels can run at once for a moment; Cloudflare splits traffic between them.

The demo cluster is disposable: recreate it from `/home/kubesre-cluster/demo/` (also kept in
`~/ks-old/demo-setup` on the laptop the move was done from), mint a new kubeconfig for the
`sre-agent/kube-sre` ServiceAccount, and keep `CLUSTER_ID` pinned.

## Notes

- `METRICS_ENABLED=false` in `.env`: nothing scrapes kube-sre, and the tunnel's path rule in
  Cloudflare still routes `/metrics`, which made it public. It now answers 404. Remove `metrics`
  from that rule too if you ever turn it back on.
- Use a model API key that belongs to kube-sre alone, with a spend limit at the provider. To
  replace it without it ever appearing on screen, run this in your own terminal:

  ```bash
  ssh -t root@45.130.104.143 'read -rsp "New API key: " K; echo; sed -i "s|^OPENAI_API_KEY=.*|OPENAI_API_KEY=$K|" /home/kubesre/.kube-sre/.env && systemctl restart kube-sre && echo done'
  ```
