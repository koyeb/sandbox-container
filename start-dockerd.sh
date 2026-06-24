#!/bin/sh
set -x

# Set up NAT for outbound traffic.
dev=$(ip route show default | awk '/default/ {print $5}')
addr=$(ip addr show dev "$dev" | awk '/inet / {split($2, a, "/"); print a[1]}')

echo 1 > /proc/sys/net/ipv4/ip_forward || true
iptables-legacy -t nat -A POSTROUTING -o "$dev" -j SNAT --to-source "$addr" -p tcp || true
iptables-legacy -t nat -A POSTROUTING -o "$dev" -j SNAT --to-source "$addr" -p udp || true

exec dockerd --iptables=false --ip6tables=false --storage-driver=vfs
