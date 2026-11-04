#!/bin/sh
# Installs Tramline on the Pi. It uses sudo there, so the user runs this
# themselves, never Claude. Run from this machine after ./build.sh:
#
#   deploy/install.sh raspberrypi
#
# It creates a "tramline" system user if needed, and installs the binary, service
# and config. It uses SSH only for this step, and is safe to re-run. It never
# creates, reads or changes secrets, and leaves an existing config alone.
set -eu

host=${1:?usage: install.sh <ssh-host>}
cd "$(dirname "$0")/.."

stage=$(ssh "$host" mktemp -d)
scp -q dist/tramline-linux-arm64 deploy/tramline.service deploy/config.json "$host:$stage/"
ssh "$host" sh -eu <<EOF
cd '$stage'
id tramline >/dev/null 2>&1 || sudo -n useradd --system --no-create-home --shell /usr/sbin/nologin --groups dialout tramline
sudo -n install -d -o tramline -g tramline -m 700 /var/lib/tramline
sudo -n install -d -o root -g root -m 755 /etc/tramline
sudo -n test -e /etc/tramline/config.json || sudo -n install -o root -g root -m 644 config.json /etc/tramline/config.json
sudo -n install -o root -g root -m 755 tramline-linux-arm64 /usr/local/bin/tramline
sudo -n install -o root -g root -m 644 tramline.service /etc/systemd/system/tramline.service
sudo -n systemctl daemon-reload
cd /
rm -rf '$stage'
if systemctl is-active --quiet tramline; then sudo -n systemctl restart tramline; fi
EOF
echo "Installed on $host"
