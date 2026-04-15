#!/usr/bin/env bash
# Add/remove fake 10.0.100.5/24 IP for cheesy-arena to bind to.
# Usage: sudo ./fake_fms_ip.sh up|down

set -euo pipefail

IFACE=dummy0
ADDR=10.0.100.5/24

case "${1:-}" in
  up)
    if ! ip link show "$IFACE" >/dev/null 2>&1; then
      ip link add "$IFACE" type dummy
    fi
    ip addr add "$ADDR" dev "$IFACE" 2>/dev/null || true
    ip link set "$IFACE" up
    ip addr show "$IFACE"
    ;;
  down)
    ip link del "$IFACE" 2>/dev/null || true
    echo "removed $IFACE"
    ;;
  *)
    echo "usage: sudo $0 up|down" >&2
    exit 1
    ;;
esac
