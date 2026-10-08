#!/usr/bin/env bash
# Install the IC-9700 rigctld unit on scmino (192.168.1.178).
# Needs libhamlib-utils on the target; installed here if missing.
set -euo pipefail
SSH_HOST="${SSH_HOST:-io@192.168.1.178}"
cd "$(dirname "${BASH_SOURCE[0]}")"
scp -q rigctld-ic9700.service "${SSH_HOST}:/tmp/rigctld-ic9700.service"
ssh "$SSH_HOST" 'set -e
command -v rigctld >/dev/null || sudo apt-get install -y -q libhamlib-utils
sudo install -m 0644 /tmp/rigctld-ic9700.service /etc/systemd/system/rigctld-ic9700.service
rm -f /tmp/rigctld-ic9700.service
sudo systemctl daemon-reload
sudo systemctl enable rigctld-ic9700.service
sudo systemctl restart rigctld-ic9700.service
sleep 1
systemctl --no-pager status rigctld-ic9700.service | head -8'
