#!/usr/bin/env bash
# Phase 1 of docs/AGENT-FLEET.md: prepare a fleet host. Run with sudo, once.
#
#   sudo packaging/fleet/host-bootstrap.sh kali --pc2-ip 192.168.1.20 \
#        [--build-pubkey "ssh-ed25519 AAAA… fleet-build"]
#   packaging/fleet/host-bootstrap.sh docker-dropin --code /path/to/code   # prints the Docker drop-in; no root
#   sudo ./host-bootstrap.sh pc2 --kali-ip 192.168.1.10 --code /home/<you>/code \
#        --pubkey "ssh-ed25519 AAAA… you@kali"
#
# kali: Docker and compose from Kali's repos, NFSv4 export of the code
#       directory to PC2 only, held packages, a weekly `git gc` for every repo
#       (the image turns gc.auto off, so eight agents never gc one .git at once).
#       With --build-pubkey, a `fleetbuild` user whose key can do nothing but
#       reach Docker (agents' DOCKER_HOST and kbuild). Docker access is root
#       here: an agent holding that key can do anything on this machine.
# pc2:  for a minimal Debian 13 or Ubuntu server (with a docker.service drop-in so Docker waits for
#       the code mount after a reboot): Docker CE, a key-only `fleet` user in the docker group
#       (docker group ≈ root), the code directory over NFS at the SAME absolute
#       path as on Kali, suspend masked, security-only unattended upgrades.
#
# Then, from Kali:  docker context create pc2 --docker host=ssh://fleet@<pc2>
# Verify:           docker --context pc2 ps
#                   touch a file in the code dir on PC2; it shows on Kali, yours.
set -euo pipefail

die() { echo "host-bootstrap: $*" >&2; exit 1; }

# docker_mount_dropin CODE_DIR: the systemd drop-in that makes Docker wait for the code mount.
# Docker never retries a container that failed to create, so if it starts before the NFS mount
# is up, every agent whose bind mount is under CODE_DIR stays dead after a reboot. The mount unit
# is derived from the path (config), not written into the source.
docker_mount_dropin() {
  local unit
  unit=$(systemd-escape --path --suffix=mount "$1") || die "cannot derive the mount unit for $1"
  cat <<EOF
# installed by host-bootstrap.sh: start Docker only once the shared code directory is mounted
[Unit]
Wants=$unit network-online.target
After=$unit network-online.target
EOF
}

# mount_retry_dropin: the drop-in that keeps the code mount retrying. At boot Docker starts every
# agent, each one triggers the automount, and until the network and Kali are up each attempt fails:
# five in a second hit systemd's start limit, the mount unit fails, the automount with it, and nothing
# mounts the share again until someone resets it (seen after a power cut, 2026-10-14). With no limit,
# every later access tries again: fleetd starting an agent Docker failed to start is one.
mount_retry_dropin() {
  cat <<EOF
# installed by host-bootstrap.sh: never stop retrying the shared code mount
[Unit]
StartLimitIntervalSec=0
EOF
}

mode="${1:-}"
# no root needed to see what would be installed
if [ "$mode" = docker-dropin ]; then
  [ "${2:-}" = --code ] && [ -n "${3:-}" ] || die "usage: host-bootstrap.sh docker-dropin --code <path>"
  docker_mount_dropin "$3"
  exit 0
fi
if [ "$mode" = mount-dropin ]; then
  mount_retry_dropin
  exit 0
fi
[ "$(id -u)" -eq 0 ] || die "run with sudo"
shift || true
operator="${SUDO_USER:-}"
code=""; pc2_ip=""; kali_ip=""; pubkey=""; build_pubkey=""; fleet_user=fleet
while [ $# -gt 0 ]; do
  case "$1" in
    --code) code="$2"; shift 2 ;;
    --pc2-ip) pc2_ip="$2"; shift 2 ;;
    --kali-ip) kali_ip="$2"; shift 2 ;;
    --pubkey) pubkey="$2"; shift 2 ;;
    --build-pubkey) build_pubkey="$2"; shift 2 ;;
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
  # No NFSv4 delegations: the code is used here too, and an open here of a file
  # PC2 holds one on waits for PC2 to give it back — up to lease-break-time
  # (45 s) per file when PC2 is cut off or powered down.
  echo "fs.leases-enable = 0" > /etc/sysctl.d/90-fleet-nfs.conf
  sysctl -q -p /etc/sysctl.d/90-fleet-nfs.conf
  exportfs -ra
  systemctl enable --now nfs-server

  cat > /etc/cron.d/fleet-git-gc <<EOF
# git gc for every repository in the shared code directory, weekly, as $operator.
17 4 * * 0 $operator find $code -maxdepth 2 -name .git -type d -prune -execdir git gc --quiet \\; 2>/dev/null
EOF

  if [ -n "$build_pubkey" ]; then
    id fleetbuild >/dev/null 2>&1 || useradd -m -s /bin/sh fleetbuild
    usermod -aG docker fleetbuild
    bh=$(getent passwd fleetbuild | cut -d: -f6)
    install -d -m 700 -o fleetbuild -g fleetbuild "$bh/.ssh"
    # The docker CLI's ssh transport runs exactly this; nothing else is allowed.
    echo "restrict,command=\"docker system dial-stdio\" $build_pubkey" > "$bh/.ssh/authorized_keys"
    chown fleetbuild:fleetbuild "$bh/.ssh/authorized_keys"; chmod 600 "$bh/.ssh/authorized_keys"
    systemctl is-active --quiet ssh || systemctl enable --now ssh
  fi

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
  . /etc/os-release
  case "$ID" in debian|ubuntu) ;; *) die "pc2 mode supports Debian and Ubuntu, not $ID" ;; esac
  curl -fsSL "https://download.docker.com/linux/$ID/gpg" -o /etc/apt/keyrings/docker.asc
  chmod a+r /etc/apt/keyrings/docker.asc
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/$ID $VERSION_CODENAME stable" \
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
# fleetd and the console open one ssh connection per agent at once; the default
# 10 unauthenticated connections drops some of eight agents' polls, and a
# dropped source is penalised, so Kali is exempt.
MaxStartups 100:30:200
PerSourcePenaltyExemptList $kali_ip
EOF
  sshd -t || die "sshd rejected /etc/ssh/sshd_config.d/10-fleet.conf"
  systemctl reload ssh

  mkdir -p "$code"
  fstab="$kali_ip:$code $code nfs4 _netdev,x-systemd.automount,noatime 0 0"
  grep -qF "$kali_ip:$code " /etc/fstab || echo "$fstab" >> /etc/fstab
  # Docker must not start before the code mount exists (see docker_mount_dropin).
  install -d -m 0755 /etc/systemd/system/docker.service.d
  docker_mount_dropin "$code" > /etc/systemd/system/docker.service.d/fleet-code-mount.conf
  # Nor may the mount give up for good before Kali is up (see mount_retry_dropin).
  mount_unit=$(systemd-escape --path --suffix=mount "$code") || die "cannot derive the mount unit for $code"
  install -d -m 0755 "/etc/systemd/system/$mount_unit.d"
  mount_retry_dropin > "/etc/systemd/system/$mount_unit.d/fleet-retry.conf"
  systemctl daemon-reload
  mount "$code" 2>/dev/null || true

  systemctl mask sleep.target suspend.target hibernate.target hybrid-sleep.target >/dev/null
  # The default unattended-upgrades origins are the security pocket only.
  echo 'APT::Periodic::Update-Package-Lists "1"; APT::Periodic::Unattended-Upgrade "1";' \
    > /etc/apt/apt.conf.d/20auto-upgrades
  echo "pc2 ready. From Kali: docker context create pc2 --docker host=ssh://$fleet_user@<this host>"
  echo "Containers run with restart: unless-stopped, so they come back at boot with docker."
  ;;

*)
  die "usage: host-bootstrap.sh kali --pc2-ip <ip> | pc2 --kali-ip <ip> --code <path> --pubkey <key>"
  ;;
esac
