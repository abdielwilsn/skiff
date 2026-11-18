#!/usr/bin/env bash
# Install or update Skiff on an Ubuntu server.
#
#   curl -fsSL https://raw.githubusercontent.com/abdielwilsn/skiff/main/scripts/install.sh | sudo bash
#
# Run the same command again to update to the latest release. Options:
#
#   ... | sudo bash -s -- --version v0.2.0   install a specific release
#   ... | sudo bash -s -- --port 9000        panel port for a fresh install (default 8090)
#   sudo bash install.sh ./dist/skiff-linux-amd64   install a locally built binary
#
# Your data (/var/lib/skiff) and an existing service file are kept on update.
set -euo pipefail

REPO="abdielwilsn/skiff"
BIN=/usr/local/bin/skiff
DATA=/var/lib/skiff
UNIT=/etc/systemd/system/skiff.service
TMP="" # download dir, removed on exit (global so the EXIT trap can see it)

say()  { printf '\033[1m==>\033[0m %s\n' "$*"; }
fail() { printf 'Error: %s\n' "$*" >&2; exit 1; }

main() {
    local version="${SKIFF_VERSION:-latest}" port="${SKIFF_PORT:-8090}" local_bin=""
    while [ $# -gt 0 ]; do
        case "$1" in
            --version) version="${2:?--version needs a value, e.g. v0.2.0}"; shift 2 ;;
            --port) port="${2:?--port needs a value}"; shift 2 ;;
            -h|--help) sed -n '2,13p' "$0" 2>/dev/null || true; exit 0 ;;
            *) local_bin="$1"; shift ;;
        esac
    done

    [ "$(id -u)" -eq 0 ] || fail "please run as root (pipe into 'sudo bash')"
    case "$port" in ''|*[!0-9]*) fail "invalid port: $port" ;; esac
    local arch
    case "$(uname -m)" in
        x86_64|amd64) arch=amd64 ;;
        aarch64|arm64) arch=arm64 ;;
        *) fail "unsupported CPU architecture: $(uname -m)" ;;
    esac

    TMP="$(mktemp -d)"
    trap 'rm -rf "$TMP"' EXIT

    if [ -n "$local_bin" ]; then
        [ -f "$local_bin" ] || fail "no such file: $local_bin"
        say "Using local binary $local_bin"
        cp "$local_bin" "$TMP/skiff"
    else
        download "$version" "$arch" "$TMP"
    fi
    chmod 0755 "$TMP/skiff"
    local new_version
    new_version="$("$TMP/skiff" version)" || fail "the downloaded binary doesn't run on this machine"

    local old_version=""
    if [ -x "$BIN" ]; then
        old_version="$("$BIN" version 2>/dev/null || echo unknown)"
    fi
    if [ -n "$old_version" ] && [ "$old_version" = "$new_version" ] && [ -z "$local_bin" ] && [ -z "${SKIFF_FORCE:-}" ]; then
        say "Skiff $new_version is already installed and up to date."
        exit 0
    fi

    # Atomic replace: a running Skiff keeps its old file until it restarts.
    say "Installing Skiff $new_version to $BIN"
    install -m 0755 "$TMP/skiff" "$BIN.new"
    mv -f "$BIN.new" "$BIN"
    install -d -m 0700 "$DATA"

    if [ ! -d /run/systemd/system ]; then
        say "systemd isn't running here, so no service was set up."
        echo "Start Skiff yourself with: $BIN serve --addr :$port --data $DATA"
        exit 0
    fi

    if [ ! -f "$UNIT" ]; then
        say "Creating the skiff service (port $port)"
        write_unit "$port"
        open_firewall "$port"
    fi
    systemctl daemon-reload
    systemctl enable skiff >/dev/null 2>&1
    systemctl restart skiff

    local i
    for i in $(seq 1 20); do
        systemctl is-active --quiet skiff && break
        sleep 0.5
    done
    systemctl is-active --quiet skiff || fail "the service didn't start; see: journalctl -u skiff -n 50"
    sleep 1

    echo
    if [ -n "$old_version" ]; then
        say "Updated Skiff: $old_version -> $new_version"
        echo "Your sites, settings and login are unchanged. Reload the panel in your browser."
        exit 0
    fi

    local ip
    ip="$(hostname -I 2>/dev/null | awk '{print $1}')"
    ip="${ip:-<server-ip>}"
    port="$(grep -oE -- '--addr [^ ]*:[0-9]+' "$UNIT" | grep -oE '[0-9]+$' || echo "$port")"
    say "Skiff $new_version is running."
    if [ -f "$DATA/setup-token" ]; then
        echo "Finish setup at: http://$ip:$port/setup?token=$(cat "$DATA/setup-token")"
        echo "Safer: ssh -L $port:localhost:$port root@$ip  then open the link with 'localhost' instead of $ip."
    else
        echo "Open: http://$ip:$port"
    fi
    echo "After setup, give the panel a domain under Settings -> Panel address & HTTPS."
    echo "Update later by running this same command again. Logs: journalctl -u skiff -f"
}

download() {
    local version="$1" arch="$2" dir="$3" base
    command -v curl >/dev/null 2>&1 || fail "curl is required"
    command -v sha256sum >/dev/null 2>&1 || fail "sha256sum is required"
    if [ -n "${SKIFF_DOWNLOAD_BASE:-}" ]; then
        base="$SKIFF_DOWNLOAD_BASE"
    elif [ "$version" = "latest" ]; then
        base="https://github.com/$REPO/releases/latest/download"
    else
        base="https://github.com/$REPO/releases/download/$version"
    fi
    local file="skiff-linux-$arch"
    say "Downloading $file ($version)"
    curl -fsSL --retry 4 --retry-all-errors --retry-delay 2 -o "$dir/$file" "$base/$file" || fail "download failed: $base/$file"
    curl -fsSL --retry 4 --retry-all-errors --retry-delay 2 -o "$dir/SHA256SUMS" "$base/SHA256SUMS" || fail "download failed: $base/SHA256SUMS"
    say "Verifying checksum"
    (cd "$dir" && grep " $file\$" SHA256SUMS | sha256sum -c --quiet -) || fail "checksum mismatch for $file; not installing"
    mv "$dir/$file" "$dir/skiff"
}

write_unit() {
    cat > "$UNIT" <<UNIT
[Unit]
Description=Skiff server panel
After=network-online.target
Wants=network-online.target

[Service]
# Skiff installs packages and manages Nginx, PHP-FPM and the site user, so it
# runs as root. Sites themselves run as the unprivileged "skiff" user.
User=root
ExecStart=$BIN serve --addr :$1 --data $DATA
Restart=on-failure
RestartSec=3
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
UNIT
}

open_firewall() {
    if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q "Status: active"; then
        say "Opening ports $1 (panel), 80 and 443 (sites) in ufw"
        ufw allow "$1/tcp" >/dev/null
        ufw allow 80/tcp >/dev/null
        ufw allow 443/tcp >/dev/null
    fi
}

# Everything runs from here, so a partially downloaded script never executes.
main "$@"
