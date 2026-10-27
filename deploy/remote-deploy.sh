#!/bin/bash
# The forced command of the CD key, run as kubesre: reads a release tarball on stdin,
# installs it, restarts kube-sre, and rolls back if it does not come up healthy.
# Installed root owned at /home/kubesre/deploy/deploy.sh; see DEPLOY.md.
set -euo pipefail

H=/home/kubesre
IN=$H/.deploy-incoming
LOCK=/var/lock/droplet-deploy.lock

# The shared lock keeps two deploys on this server from running at once.
exec 9<"$LOCK"
flock -w 900 9

rm -rf "$IN"
mkdir -m 700 "$IN"
tar -xzf - -C "$IN"

test -x "$IN/kube-sre" && test -f "$IN/site/index.html"
"$IN/kube-sre" version
rev=$(cat "$IN/REVISION" 2>/dev/null || echo unknown)

healthy() {
	local old=$1 pid
	for _ in $(seq 1 60); do
		pid=$(pgrep -u kubesre -x kube-sre || true)
		if [ -n "$pid" ] && [ "$pid" != "$old" ] &&
			curl -fs -m 2 http://127.0.0.1:8765/healthz | grep -q '"status":"ok"' &&
			curl -fs -m 2 -o /dev/null http://127.0.0.1:9000/; then
			return 0
		fi
		sleep 1
	done
	return 1
}

# The unit has Restart=always, so stopping the process restarts it with the new binary.
restart() {
	local old
	old=$(pgrep -u kubesre -x kube-sre || true)
	pkill -TERM -u kubesre -x kube-sre || true
	healthy "$old"
}

cp -a "$H/bin/kube-sre" "$H/bin/kube-sre.previous"
rm -rf "$H/site.previous"
cp -a "$H/site" "$H/site.previous"

install -m 750 "$IN/kube-sre" "$H/bin/kube-sre.new"
mv -f "$H/bin/kube-sre.new" "$H/bin/kube-sre"
rm -rf "$H/site.new"
cp -a "$IN/site" "$H/site.new"
find "$H/site.new" -type d -exec chmod 750 {} +
find "$H/site.new" -type f -exec chmod 640 {} +
rm -rf "$H/site.old"
mv "$H/site" "$H/site.old"
mv "$H/site.new" "$H/site"
rm -rf "$H/site.old" "$IN"

if restart; then
	echo "deployed $rev"
	exit 0
fi

echo "new version did not come up healthy, rolling back" >&2
install -m 750 "$H/bin/kube-sre.previous" "$H/bin/kube-sre"
rm -rf "$H/site"
cp -a "$H/site.previous" "$H/site"
if restart; then
	echo "rolled back to the previous version" >&2
else
	echo "rollback did not come up healthy either; check journalctl -u kube-sre" >&2
fi
exit 1
