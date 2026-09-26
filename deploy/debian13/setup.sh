#!/usr/bin/env bash
set -euo pipefail

APP_DIR="/opt/videos"
SERVICE_USER="videos"
HOSTNAME_NEW="video-server"
STATIC_IP="10.120.1.24/24"
STATIC_IP_RAW="10.120.1.24"
SSH_PORT="22"
VIDEO_PORT="8090"

if [[ "$EUID" -ne 0 ]]; then
  echo "Run as root."
  exit 1
fi

echo "== Current network =="
ip -br address
echo
ip route

IFACE="$(ip -o -4 route show default | awk 'NR==1 {print $5}')"
GATEWAY="$(ip -o -4 route show default | awk 'NR==1 {print $3}')"

if [[ -z "$IFACE" || -z "$GATEWAY" ]]; then
  echo "Cannot detect active interface/default gateway."
  exit 1
fi

echo
echo "Detected interface: $IFACE"
echo "Detected gateway:   $GATEWAY"
echo "Target address:     $STATIC_IP"

if ip -4 addr show dev "$IFACE" | grep -q " $STATIC_IP_RAW/"; then
  echo "Static IP already present on $IFACE."
fi

echo
echo "== Packages =="
apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y \
  ca-certificates \
  curl \
  ffmpeg \
  git \
  golang-go \
  nftables \
  sudo

echo
echo "== Hostname =="
hostnamectl set-hostname "$HOSTNAME_NEW"
if ! grep -qE "^127\\.0\\.1[[:space:]]+$HOSTNAME_NEW$" /etc/hosts; then
  printf '127.0.1.1 %s\n' "$HOSTNAME_NEW" >> /etc/hosts
fi

echo
echo "== Service user =="
if ! id -u "$SERVICE_USER" >/dev/null 2>&1; then
  useradd --system --home "$APP_DIR" --shell /usr/sbin/nologin "$SERVICE_USER"
fi
install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0755 "$APP_DIR"

echo
echo "== SSH baseline =="
install -d -m 0755 /etc/ssh/sshd_config.d
cat >/etc/ssh/sshd_config.d/20-videos.conf <<EOF
Port $SSH_PORT
PermitRootLogin prohibit-password
PasswordAuthentication yes
KbdInteractiveAuthentication no
X11Forwarding no
EOF

sshd -t
systemctl reload ssh || systemctl restart ssh

echo
echo "== systemd-networkd static network =="
install -d -m 0755 /etc/systemd/network
STAMP="$(date +%Y%m%d-%H%M%S)"
if [[ -f /etc/network/interfaces ]]; then
  cp -a /etc/network/interfaces "/etc/network/interfaces.backup.$STAMP"
fi
if [[ -d /etc/network/interfaces.d ]]; then
  cp -a /etc/network/interfaces.d "/etc/network/interfaces.d.backup.$STAMP"
fi

cat >"/etc/systemd/network/10-video-server.network" <<EOF
[Match]
Name=$IFACE

[Network]
Address=$STATIC_IP
Gateway=$GATEWAY
DNS=$GATEWAY
DNS=1.1.1.1
IPv6AcceptRA=no
EOF

# If NetworkManager is active, stop it before enabling networkd.
if systemctl is-active --quiet NetworkManager; then
  systemctl disable --now NetworkManager
fi

systemctl enable systemd-networkd
systemctl enable systemd-resolved
ln -sf /run/systemd/resolve/stub-resolv.conf /etc/resolv.conf || true

echo
echo "Network file written. The network will be switched to $STATIC_IP on reboot."
echo "Reboot is intentionally NOT automatic."
echo

echo "== nftables =="
install -m 0644 "$(dirname "$0")/nftables.conf" /etc/nftables.conf
nft -c -f /etc/nftables.conf
systemctl enable nftables

echo
echo "== systemd service =="
install -m 0644 "$(dirname "$0")/video-core.service" /etc/systemd/system/video-core.service
systemctl daemon-reload
systemctl enable video-core

echo
echo "============================================"
echo "Debian 13 baseline prepared."
echo "Interface : $IFACE"
echo "Gateway   : $GATEWAY"
echo "Static IP : $STATIC_IP_RAW/24"
echo "SSH       : $SSH_PORT/tcp"
echo "Video UI  : $VIDEO_PORT/tcp"
echo
echo "IMPORTANT: reboot from a local console/ESXi console:"
echo "  reboot"
echo
echo "After reboot:"
echo "  ip -br a"
echo "  ip route"
echo "  systemctl status systemd-networkd --no-pager"
echo "  systemctl status nftables --no-pager"
echo "  systemctl status video-core --no-pager"
echo "============================================"
