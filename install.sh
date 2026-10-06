#!/bin/bash
# Private Label Portal install script for a fresh Ubuntu server (24.04 tested).
# Run from the cloned repo:  sudo ./install.sh
# Safe to re-run: it rebuilds, keeps the database, files and configuration,
# and restarts the service (this is also how to upgrade after a git pull).
set -euo pipefail

APP=plportal
BIN=/usr/local/bin/plportal
DATA_DIR=/var/lib/plportal
CONF_DIR=/etc/plportal
CONF=$CONF_DIR/plportal.env
GO_DIR=/usr/local/go

log()  { echo "[install] $*"; }
warn() { echo "[install] WARNING: $*" >&2; }
die()  { echo "[install] ERROR: $*" >&2; exit 1; }

ask() { # ask VAR "prompt" default
  local __var=$1 __prompt=$2 __def=${3:-} __ans
  if [ -n "$__def" ]; then read -r -p "$__prompt [$__def]: " __ans; else read -r -p "$__prompt: " __ans; fi
  printf -v "$__var" '%s' "${__ans:-$__def}"
}

confirm() { # confirm "question" default(y/n)
  local __ans __def=${2:-y}
  read -r -p "$1 [$( [ "$__def" = y ] && echo Y/n || echo y/N )]: " __ans
  __ans=${__ans:-$__def}
  [[ "$__ans" =~ ^[Yy] ]]
}

[ "$(id -u)" -eq 0 ] || die "run as root: sudo ./install.sh"
cd "$(dirname "$0")"
[ -f go.mod ] && [ -d cmd/plportal ] || die "run this from the PrivateLabel_Portal repository"

# --- Packages. A minimal Ubuntu image may lack ca-certificates, which
# breaks every HTTPS call including Go's module downloads.
NEED=()
for p in ca-certificates curl git; do dpkg -s "$p" >/dev/null 2>&1 || NEED+=("$p"); done
if [ ${#NEED[@]} -gt 0 ]; then
  log "installing: ${NEED[*]}"
  apt-get update -qq
  apt-get install -y -qq "${NEED[@]}"
fi

# --- Configuration: DNS name, Let's Encrypt email, time zone. Saved answers
# are offered as defaults on re-runs, so Enter keeps them.
PLP_DOMAIN=${PLP_DOMAIN:-}
PLP_EMAIL=${PLP_EMAIL:-}
PLP_TIMEZONE=${PLP_TIMEZONE:-Europe/Sarajevo}
PLP_MAX_MB=${PLP_MAX_MB:-3072}
if [ -f "$CONF" ]; then
  # shellcheck disable=SC1090
  . "$CONF"
fi
echo
echo "The portal gets its HTTPS certificate from Let's Encrypt. It needs:"
echo "  - this server's public DNS name (an existing A record pointing at this server), and"
echo "  - an email address for Let's Encrypt (expiry and account notices)."
echo
while :; do
  ask PLP_DOMAIN "Public DNS name" "$PLP_DOMAIN"
  PLP_DOMAIN=$(echo "$PLP_DOMAIN" | tr 'A-Z' 'a-z' | sed 's/\.$//')
  [[ "$PLP_DOMAIN" =~ ^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,}$ ]] && break
  echo "  That doesn't look like a DNS name (example: branding.example.com)."
done
while :; do
  ask PLP_EMAIL "Email address for Let's Encrypt" "$PLP_EMAIL"
  [[ "$PLP_EMAIL" =~ ^[^[:space:]@]+@[^[:space:]@]+\.[^[:space:]@]+$ ]] && break
  echo "  That doesn't look like an email address."
done
while :; do
  ask PLP_TIMEZONE "Time zone for displayed times and folder names" "$PLP_TIMEZONE"
  [ -f "/usr/share/zoneinfo/$PLP_TIMEZONE" ] && break
  echo "  Unknown time zone. Examples: Europe/Sarajevo, America/Chicago, UTC."
done

# Check the A record against this server's addresses. Behind 1:1 NAT the
# public address isn't configured locally, so this only warns.
RESOLVED=$(getent ahostsv4 "$PLP_DOMAIN" | awk '{print $1}' | sort -u | tr '\n' ' ' || true)
LOCAL=$(hostname -I 2>/dev/null || true)
if [ -z "$RESOLVED" ]; then
  warn "$PLP_DOMAIN does not resolve. Let's Encrypt will fail until the A record exists."
  confirm "Continue anyway?" n || exit 1
else
  MATCH=0
  for ip in $RESOLVED; do [[ " $LOCAL " == *" $ip "* ]] && MATCH=1; done
  if [ $MATCH -eq 0 ]; then
    warn "$PLP_DOMAIN resolves to $RESOLVED, which is not an address on this server ($LOCAL)."
    echo "  That is fine behind NAT, as long as ports 80 and 443 of that address reach this server."
    confirm "Continue?" y || exit 1
  fi
fi

install -d -m 755 "$CONF_DIR"
cat > "$CONF" <<EOF
PLP_DOMAIN=$PLP_DOMAIN
PLP_EMAIL=$PLP_EMAIL
PLP_TIMEZONE=$PLP_TIMEZONE
PLP_MAX_MB=$PLP_MAX_MB
EOF
chmod 644 "$CONF"
log "configuration saved to $CONF"

# --- No host firewall changes: the SERVERware platform firewall in front of
# this VPS controls access. Ports 80 and 443 must be allowed there.


# --- Go toolchain.
export PATH="$PATH:$GO_DIR/bin"
if ! command -v go >/dev/null 2>&1; then
  GOVER=$(curl -fsSL "https://go.dev/VERSION?m=text" | head -1)
  ARCH=$(dpkg --print-architecture)
  log "installing Go $GOVER ($ARCH)"
  TMP=$(mktemp)
  curl -fsSL -o "$TMP" "https://go.dev/dl/${GOVER}.linux-${ARCH}.tar.gz"
  rm -rf "$GO_DIR"
  tar -C /usr/local -xzf "$TMP"
  rm -f "$TMP"
fi
log "using $(go version)"

# --- Build.
VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
log "building plportal $VERSION"
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$BIN.new" ./cmd/plportal
install -m 755 "$BIN.new" "$BIN"
rm -f "$BIN.new"

# --- Service user and data directory. The app never runs as root.
if ! id "$APP" >/dev/null 2>&1; then
  useradd --system --home-dir "$DATA_DIR" --no-create-home --shell /usr/sbin/nologin "$APP"
  log "created system user $APP"
fi
install -d -m 700 -o "$APP" -g "$APP" "$DATA_DIR"

# --- First admin account (first install only). Printed on exit however the
# script ends, since it is shown only once.
CREDS=""
print_creds() {
  [ -n "$CREDS" ] || return 0
  echo
  echo "================================================================"
  echo "$CREDS"
  echo "Log in at: https://$PLP_DOMAIN/#admin"
  echo "================================================================"
  echo
  log "save this password now; it is not shown again."
  log "(new password later: sudo runuser -u $APP -- $BIN reset-password -data-dir $DATA_DIR -username <name>)"
}
trap print_creds EXIT
if [ ! -f "$DATA_DIR/plportal.db" ]; then
  ADMIN_USER=admin
  ADMIN_EMAIL=$PLP_EMAIL
  ask ADMIN_USER "Username for the first admin account" "$ADMIN_USER"
  ask ADMIN_EMAIL "Email for that admin (submission notifications)" "$ADMIN_EMAIL"
  CREDS=$(runuser -u "$APP" -- "$BIN" create-admin -data-dir "$DATA_DIR" -username "$ADMIN_USER" -email "$ADMIN_EMAIL")
fi

# --- systemd units.
HARDENING='NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectKernelLogs=true
ProtectControlGroups=true
ProtectClock=true
ProtectHostname=true
ProtectProc=invisible
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=true
RestrictRealtime=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true
SystemCallArchitectures=native
SystemCallFilter=@system-service
UMask=0077
ReadWritePaths='"$DATA_DIR"

cat > /etc/systemd/system/$APP.service <<EOF
[Unit]
Description=Private Label Portal
Wants=network-online.target
After=network-online.target

[Service]
User=$APP
Group=$APP
EnvironmentFile=$CONF
ExecStart=$BIN serve -domain \${PLP_DOMAIN} -email \${PLP_EMAIL} -timezone \${PLP_TIMEZONE} -max-submission-mb \${PLP_MAX_MB} -data-dir $DATA_DIR
Restart=always
RestartSec=2
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
$HARDENING

[Install]
WantedBy=multi-user.target
EOF

cat > /etc/systemd/system/$APP-backup.service <<EOF
[Unit]
Description=Private Label Portal database backup

[Service]
Type=oneshot
User=$APP
Group=$APP
ExecStart=$BIN backup -data-dir $DATA_DIR -keep 14
CapabilityBoundingSet=
$HARDENING
EOF

cat > /etc/systemd/system/$APP-backup.timer <<EOF
[Unit]
Description=Daily Private Label Portal database backup

[Timer]
OnCalendar=daily
RandomizedDelaySec=1h
Persistent=true

[Install]
WantedBy=timers.target
EOF

systemctl daemon-reload
systemctl enable --now $APP-backup.timer >/dev/null
systemctl enable $APP >/dev/null 2>&1
systemctl restart $APP
log "service $APP started (systemctl status $APP; journalctl -u $APP)"
log "daily database backups to $DATA_DIR/backups (last 14 kept); submission files are in $DATA_DIR/submissions"

# --- First HTTPS request: this is when the certificate is requested.
log "requesting the certificate for https://$PLP_DOMAIN/ (can take up to a minute)"
CODE=000
for _ in $(seq 1 20); do
  CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 20 "https://$PLP_DOMAIN/" || true)
  [ "$CODE" = 200 ] && break
  sleep 3
done
if [ "$CODE" = 200 ]; then
  log "HTTPS works: https://$PLP_DOMAIN/"
else
  warn "https://$PLP_DOMAIN/ is not answering with a valid certificate yet."
  echo "  Check that the A record points here and ports 80 and 443 are reachable from the internet,"
  echo "  then look at: journalctl -u $APP -n 50"
fi
