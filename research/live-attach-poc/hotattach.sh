#!/bin/sh
# Proof-of-concept: hot-attach/detach a device node to/from an ALREADY-RUNNING
# container, with zero pod recreation. Run this ON THE NODE (e.g. via
# `docker exec <kind-node> sh /path/to/this/script ...` for a kind cluster,
# or directly on a real node's host shell) -- it needs nsenter + crictl and
# root, same privilege level config/dra's own driver DaemonSets already run
# at.
#
# This is a research artifact proving one primitive, not a product: see
# README.md for exactly what this does and does not establish.
#
# Usage:
#   hotattach.sh attach   <container-id> <device-name> <major> <minor>
#   hotattach.sh detach   <container-id> <device-name>
#
# Example (the fake-GPU stand-in used in README.md's walkthrough, matching
# /dev/urandom's major:minor so the node is actually readable, not just a
# dangling special file):
#   hotattach.sh attach <container-id> gnwes-fake-gpu 1 9
#   hotattach.sh detach <container-id> gnwes-fake-gpu

set -eu

op="${1:-}"
cid="${2:-}"
devname="${3:-}"

if [ -z "$op" ] || [ -z "$cid" ] || [ -z "$devname" ]; then
  echo "usage: $0 attach|detach <container-id> <device-name> [major] [minor]" >&2
  exit 2
fi

pid="$(crictl inspect "$cid" | grep -o '"pid": *[0-9]*' | tail -1 | grep -o '[0-9]*$')"
if [ -z "$pid" ]; then
  echo "could not resolve a PID for container $cid via crictl inspect" >&2
  exit 1
fi

case "$op" in
  attach)
    major="${4:?major device number required for attach}"
    minor="${5:?minor device number required for attach}"
    nsenter -t "$pid" -m -- mknod "/dev/$devname" c "$major" "$minor"
    nsenter -t "$pid" -m -- chmod 0666 "/dev/$devname"
    echo "attached /dev/$devname (char $major:$minor) into container $cid (pid $pid), live"
    ;;
  detach)
    nsenter -t "$pid" -m -- rm -f "/dev/$devname"
    echo "detached /dev/$devname from container $cid (pid $pid), live"
    ;;
  *)
    echo "unknown operation: $op (expected attach or detach)" >&2
    exit 2
    ;;
esac
