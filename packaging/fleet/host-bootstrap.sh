#!/usr/bin/env bash
# Phase 1 of docs/AGENT-FLEET.md: prepare a fleet host. Run with sudo, once.
#
#   sudo packaging/fleet/host-bootstrap.sh kali --pc2-ip 192.168.1.20
#   sudo ./host-bootstrap.sh pc2 --kali-ip 192.168.1.10 --code /home/<you>/code \
#        --pubkey "ssh-ed25519 AAAA… you@kali"
#
# kali: Docker and compose from Kali's repos, NFSv4 export of the code
#       directory to PC2 only, held packages, a weekly `git gc` for every repo
#       (the image turns gc.auto off, so eight agents never gc one .git at once).
# pc2:  for a Debian 13 netinst with only "SSH server" and "standard system
#       utilities": Docker CE, a key-only `fleet` user in the docker group
#       (docker group ≈ root), the code directory over NFS at the SAME absolute
#       path as on Kali, suspend masked, security-only unattended upgrades.
#
# Then, from Kali:  docker context create pc2 --docker host=ssh://fleet@<pc2>
# Verify:           docker --context pc2 ps
#                   touch a file in the code dir on PC2; it shows on Kali, yours.
set -euo pipefail

die() { echo "host-bootstrap: $*" >&2; exit 1; }
[ "$(id -u)" -eq 0 ] || die "run with sudo"
mode="${1:-}"; shift || true
operator="${SUDO_USER:-}"
code=""; pc2_ip=""; kali_ip=""; pubkey=""; fleet_user=fleet
while [ $# -gt 0 ]; do
  case "$1" in
    --code) code="$2"; shift 2 ;;
    --pc2-ip) pc2_ip="$2"; shift 2 ;;
    --kali-ip) kali_ip="$2"; shift 2 ;;
    --pubkey) pubkey="$2"; shift 2 ;;
    --user) fleet_user="$2"; shift 2 ;;
    *) die "unknown argument $1" ;;
  esac
done

case "$mode" in
kali)
  [ -n "$operator" ] || die "run through sudo as yourself, so the code directory is yours"
  [ -n "$pc2_ip" ] || die "--pc2-ip is required (the export is for PC2 only)"
  home=$(getent passwd "$operator" | cut -d: -f6)
  code="${code:-$home/code}"
  [ -d "$code" ] || die "$code does not exist"

  apt-get update
  apt-get install -y docker.io docker-compose nfs-kernel-server git jq
  usermod -aG docker "$operator"
  # Kali is rolling: an upgrade mid-task can restart the daemon under eight agents.
  apt-mark hold docker.io containerd runc nfs-kernel-server >/dev/null

  line="$code $pc2_ip(rw,sync,no_subtree_check)"
  grep -qxF "$line" /etc/exports 2>/dev/null || echo "$line" >> /etc/exports
  exportfs -ra
  systemctl enable --now nfs-server

  cat > /etc/cron.d/fleet-git-gc <<EOF
# git gc for every repository in the shared code directory, weekly, as $operator.
17 4 * * 0 $operator find $code -maxdepth 2 -name .git -type d -prune -execdir git gc --quiet \\; 2>/dev/null
EOF

  if command -v ufw >/dev/null && ufw status | grep -q active; then
    ufw allow from "$pc2_ip" to any port nfs
    ufw allow from "$pc2_ip" to any port 7879 proto tcp   # fleetd relay API
    ufw allow from "$pc2_ip" to any port 4318 proto tcp   # fleetd OTLP
  else
    echo "No active ufw. Allow only $pc2_ip to reach NFS (2049), 7879 and 4318 in your firewall."
  fi
  echo "kali ready. Log out and back in for the docker group. Don't full-upgrade with agents mid-task."
  ;;

pc2)
  [ -n "$kali_ip" ] || die "--kali-ip is required"
  [ -n "$code" ] || die "--code is required: the code directory's absolute path ON KALI (it is mounted at the same path here)"
  [ -n "$pubkey" ] || die "--pubkey is required: the key Kali's docker context will use"

  apt-get update
  apt-get install -y ca-certificates curl gnupg nfs-common unattended-upgrades jq
  install -m 0755 -d /etc/apt/keyrings
  curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc
  chmod a+r /etc/apt/keyrings/docker.asc
  . /etc/os-release
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian $VERSION_CODENAME stable" \
    > /etc/apt/sources.list.d/docker.list
  apt-get update
  apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
  systemctl enable --now docker

  id "$fleet_user" >/dev/null 2>&1 || useradd -m -s /bin/bash "$fleet_user"
  usermod -aG docker "$fleet_user"
  fh=$(getent passwd "$fleet_user" | cut -d: -f6)
  install -d -m 700 -o "$fleet_user" -g "$fleet_user" "$fh/.ssh"
  grep -qxF "$pubkey" "$fh/.ssh/authorized_keys" 2>/dev/null || echo "$pubkey" >> "$fh/.ssh/authorized_keys"
  chown "$fleet_user:$fleet_user" "$fh/.ssh/authorized_keys"; chmod 600 "$fh/.ssh/authorized_keys"
  cat > /etc/ssh/sshd_config.d/10-fleet.conf <<EOF
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin no
EOF
  systemctl reload ssh

  mkdir -p "$code"
  fstab="$kali_ip:$code $code nfs4 _netdev,x-systemd.automount,noatime 0 0"
  grep -qF "$kali_ip:$code " /etc/fstab || echo "$fstab" >> /etc/fstab
  systemctl daemon-reload
  mount "$code" 2>/dev/null || true

  systemctl mask sleep.target suspend.target hibernate.target hybrid-sleep.target >/dev/null
  # Debian's default unattended-upgrades origin is the security pocket only.
  echo 'APT::Periodic::Update-Package-Lists "1"; APT::Periodic::Unattended-Upgrade "1";' \
    > /etc/apt/apt.conf.d/20auto-upgrades
  echo "pc2 ready. From Kali: docker context create pc2 --docker host=ssh://$fleet_user@<this host>"
  echo "Containers run with restart: unless-stopped, so they come back at boot with docker."
  ;;

*)
  die "usage: host-bootstrap.sh kali --pc2-ip <ip> | pc2 --kali-ip <ip> --code <path> --pubkey <key>"
  ;;
esac
