#!/usr/bin/env bash
# Installs Skiff as a systemd service on an Ubuntu server.
#
#   sudo ./scripts/install.sh                 # uses dist/skiff-linux-<arch> or ./skiff
#   sudo ./scripts/install.sh /path/to/skiff  # or an explicit binary
#
# Environment: SKIFF_PORT (default 8090)
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
    echo "Please run as root: sudo $0 $*" >&2
    exit 1
fi

case "$(uname -m)" in
    x86_64 | amd64) ARCH=amd64 ;;
    aarch64 | arm64) ARCH=arm64 ;;
    *) echo "Unsupported CPU architecture: $(uname -m)" >&2; exit 1 ;;
esac

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC="${1:-}"
if [ -z "$SRC" ]; then
    for candidate in "$HERE/../dist/skiff-linux-$ARCH" "$HERE/skiff-linux-$ARCH" "$HERE/../skiff" "$HERE/skiff"; do
        if [ -f "$candidate" ]; then SRC="$candidate"; break; fi
    done
fi
if [ -z "$SRC" ] || [ ! -f "$SRC" ]; then
    echo "Could not find a Skiff binary. Build one with 'make release' and pass its path." >&2
    exit 1
fi

PORT="${SKIFF_PORT:-8090}"
DATA=/var/lib/skiff

echo "==> Installing $SRC to /usr/local/bin/skiff"
install -m 0755 "$SRC" /usr/local/bin/skiff
install -d -m 0700 "$DATA"

echo "==> Writing systemd unit"
cat > /etc/systemd/system/skiff.service <<EOF
[Unit]
Description=Skiff server panel
After=network-online.target
Wants=network-online.target

[Service]
# Skiff installs packages and manages Nginx, PHP-FPM and the site user, so it
# runs as root. Sites themselves run as the unprivileged "skiff" user.
User=root
ExecStart=/usr/local/bin/skiff serve --addr :${PORT} --data ${DATA}
Restart=on-failure
RestartSec=3
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable skiff >/dev/null
systemctl restart skiff

if command -v ufw >/dev/null 2>&1 && ufw status | grep -q "Status: active"; then
    echo "==> Opening ports ${PORT} (panel), 80 and 443 (sites) in ufw"
    ufw allow "${PORT}/tcp" >/dev/null
    ufw allow 80/tcp >/dev/null
    ufw allow 443/tcp >/dev/null
fi

# Give the service a moment to start and write the first-run setup token.
for _ in $(seq 1 20); do
    systemctl is-active --quiet skiff && break
    sleep 0.5
done
sleep 1

IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
IP="${IP:-<server-ip>}"
echo
echo "Skiff is running: $(/usr/local/bin/skiff version)"
if [ -f "$DATA/setup-token" ]; then
    echo "Finish setup at: http://${IP}:${PORT}/setup?token=$(cat "$DATA/setup-token")"
else
    echo "Open: http://${IP}:${PORT}"
fi
echo "This address is plain HTTP. After setup, give the panel a domain under"
echo "Settings -> Panel address & HTTPS (or use: ssh -L ${PORT}:localhost:${PORT} root@${IP})."
echo "Logs: journalctl -u skiff -f"
