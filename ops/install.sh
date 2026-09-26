#!/usr/bin/env bash
# Install or upgrade Servd on an Ubuntu/Debian server.
#
#   git clone <repo> /opt/servd && cd /opt/servd && sudo ops/install.sh
#
# Re-run after `git pull` to upgrade; apps keep running during upgrades.
# Add --check to also run the isolation self-test on this machine.
set -euo pipefail

# Optional, used on first install:  APPS_DOMAIN=dynaserve.app ACME_EMAIL=you@x.com sudo -E ops/install.sh
APPS_DOMAIN="${APPS_DOMAIN:-}"
ACME_EMAIL="${ACME_EMAIL:-}"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
CHECK=false
[[ "${1:-}" == "--check" ]] && CHECK=true

say() { printf '\n\033[1;32m==>\033[0m %s\n' "$*"; }
die() { printf '\n\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root: sudo $0"
command -v apt-get >/dev/null || die "this script supports Ubuntu/Debian (apt)"
[[ "$(uname -s)" == Linux ]] || die "Servd needs Linux"

say "Installing system packages (runc, git, iptables)"
export DEBIAN_FRONTEND=noninteractive
missing=()
for pkg in runc git iptables iproute2 ca-certificates curl openssl; do
  dpkg -s "$pkg" >/dev/null 2>&1 || missing+=("$pkg")
done
if ((${#missing[@]})); then
  # A broken third-party repo shouldn't block the install; the install below
  # fails clearly if the packages themselves can't be fetched.
  apt-get update -qq || echo "warning: apt-get update reported errors; continuing"
  apt-get install -y -qq "${missing[@]}" >/dev/null || die "could not install: ${missing[*]}"
fi

say "Checking the kernel"
grep -qw overlay /proc/filesystems || modprobe overlay || die "overlayfs is not available"
[[ -e /proc/self/ns/user ]] || die "user namespaces are not available"

# Go 1.25+ is needed to build; install the official release if missing or old.
need_go=true
if command -v go >/dev/null; then
  v="$(go env GOVERSION | sed 's/^go//')"
  [[ "$(printf '%s\n1.25\n' "$v" | sort -V | head -1)" == "1.25" ]] && need_go=false
fi
export PATH="/usr/local/go/bin:$PATH"
if $need_go && ! /usr/local/go/bin/go version 2>/dev/null | grep -qE 'go1\.(2[5-9]|[3-9][0-9])'; then
  say "Installing Go"
  arch="$(dpkg --print-architecture)"   # amd64 / arm64
  ver="$(curl -fsSL 'https://go.dev/VERSION?m=text' | head -1)"
  curl -fsSL "https://go.dev/dl/${ver}.linux-${arch}.tar.gz" -o /tmp/go.tgz
  rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz && rm /tmp/go.tgz
fi

say "Building servd"
cd "$REPO"
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /usr/local/bin/servd.new ./cmd/servd
mv /usr/local/bin/servd.new /usr/local/bin/servd

say "Setting up directories and config"
install -d -m 0711 /var/lib/servd
install -d -m 0700 /etc/servd
if [[ ! -f /etc/servd/servd.env ]]; then
  host="$(curl -fsS --max-time 5 https://api.ipify.org 2>/dev/null || hostname -I | awk '{print $1}')"
  sed -e "s|__PUBLIC_HOST__|${host}|" \
      -e "s|__APPS_DOMAIN__|${APPS_DOMAIN}|" \
      -e "s|__ACME_EMAIL__|${ACME_EMAIL}|" \
      -e "s|__SESSION_SECRET__|$(openssl rand -hex 32)|" \
      -e "s|__ENCRYPTION_KEY__|$(openssl rand -hex 16)|" \
      ops/servd.env.example > /etc/servd/servd.env
  chmod 600 /etc/servd/servd.env
  echo "Created /etc/servd/servd.env (PUBLIC_HOST=${host})"
else
  echo "Keeping existing /etc/servd/servd.env"
fi

domain="$(sed -n 's/^APPS_DOMAIN=//p' /etc/servd/servd.env)"
if [[ -n "$domain" ]]; then
  # servd serves apps on 80/443 itself; nothing else may hold those ports.
  for port in 80 443; do
    holder="$(ss -Hltnp "sport = :$port" 2>/dev/null | grep -v servd | grep -oP 'users:\(\("\K[^"]+' | head -1 || true)"
    [[ -z "$holder" ]] || die "port $port is used by '$holder'; stop it (e.g. systemctl disable --now $holder) so servd can serve apps"
  done
fi
if command -v ufw >/dev/null && ufw status | grep -q "Status: active"; then
  if [[ -n "$domain" ]]; then
    say "Opening firewall ports (80/443 apps, 8080 API)"
    ufw allow 80/tcp >/dev/null
    ufw allow 443/tcp >/dev/null
  else
    say "Opening firewall ports (8080 API, 9000-9100 app URLs)"
    ufw allow 9000:9100/tcp >/dev/null
  fi
  ufw allow 8080/tcp >/dev/null
fi

if $CHECK; then
  say "Running the isolation self-test (builds and runs real sandboxes)"
  set -a; . /etc/servd/servd.env; set +a
  SERVD_INTEGRATION=1 SERVD_TEST_DIR=/var/lib/servd-selftest \
    go test -count=1 ./internal/sandbox ./internal/engine \
    || die "self-test failed; see the output above"
  rm -rf /var/lib/servd-selftest /var/lib/servd-engine-test
fi

if [[ -d /run/systemd/system ]]; then
  say "Starting the servd service"
  install -m 0644 ops/servd.service /etc/systemd/system/servd.service
  systemctl daemon-reload
  systemctl enable servd >/dev/null 2>&1
  systemctl restart servd
  for _ in $(seq 30); do curl -fs localhost:8080/health >/dev/null 2>&1 && break; sleep 1; done
  curl -fs localhost:8080/health >/dev/null || die "servd did not start: journalctl -u servd -n 50"
  journalctl -u servd -n 20 --no-pager | grep -E "runtime:|listening" || true
  . /etc/servd/servd.env
  say "Servd is running"
  if [[ -n "${APPS_DOMAIN:-}" ]]; then
    echo "  API:     https://${API_DOMAIN:-api.$APPS_DOMAIN}   (point the dashboard's NEXT_PUBLIC_PLATFORM_URL here)"
    echo "  Apps:    https://<app>-<id>.${APPS_DOMAIN}   (needs DNS: *.${APPS_DOMAIN} -> this server)"
  else
    echo "  API:     http://${PUBLIC_HOST}:8080   (point the dashboard's NEXT_PUBLIC_PLATFORM_URL here)"
    echo "  Apps:    http://${PUBLIC_HOST}:9000-9100"
  fi
  echo "  Config:  /etc/servd/servd.env   Logs: journalctl -u servd -f"
else
  say "Installed. systemd isn't running here; start it with:"
  echo "  set -a; . /etc/servd/servd.env; set +a; /usr/local/bin/servd"
fi
