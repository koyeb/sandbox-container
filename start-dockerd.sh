#!/bin/bash
set -xe -o pipefail

# Back /var/lib/docker with an ext4 disk image instead of a tmpfs so that
# nested Docker state consumes disk rather than RAM.
DOCKER_DISK_PATH="${DOCKER_DISK_PATH:-/tmp/disk.img}"
DOCKER_DISK_SIZE="${DOCKER_DISK_SIZE:-20G}"

mkdir -p /var/lib/docker
if ! mountpoint -q /var/lib/docker; then
    truncate -s "${DOCKER_DISK_SIZE}" "${DOCKER_DISK_PATH}"
    mkfs.ext4 -q -F "${DOCKER_DISK_PATH}"
    mount "${DOCKER_DISK_PATH}" /var/lib/docker
fi

# TODO: until we investigate handling of fragmented packets, match the host
# MTU in nested dockerd to make packet forwarding reliable.
dev=$(ip route show default | sed 's/.*\sdev\s\(\S*\)\s.*$/\1/')
addr=$(ip addr show dev "$dev" | grep -w inet | sed 's/^\s*inet\s\(\S*\)\/.*$/\1/')
mtu=$(ip -o link show dev "$dev" | sed -n 's/.*mtu \([0-9]*\).*/\1/p')

IPTABLES="iptables"
if command -v iptables-legacy >/dev/null 2>&1; then
    IPTABLES="iptables-legacy"
fi

echo 1 > /proc/sys/net/ipv4/ip_forward
$IPTABLES -t nat -A POSTROUTING -o "$dev" -j SNAT --to-source "$addr" -p tcp
$IPTABLES -t nat -A POSTROUTING -o "$dev" -j SNAT --to-source "$addr" -p udp

exec dockerd-entrypoint.sh --iptables=false --ip6tables=false ${mtu:+--mtu="$mtu"} -D
