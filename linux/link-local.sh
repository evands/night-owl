#!/bin/sh
# Make sure this machine has a link-local (169.254.x.x) address, so that
# broadcasts from decks on self-assigned addresses are heard. install.sh
# asks NetworkManager to keep one; this is the backstop for machines
# without NetworkManager. It prefers a wired interface, waits a little for
# the network at boot, and does nothing if the address is already there.
ADDR=169.254.57.59
pick() {
  for d in /sys/class/net/en* /sys/class/net/eth*; do
    [ -e "$d" ] || continue
    if [ "$(cat "$d/operstate" 2>/dev/null)" = up ]; then basename "$d"; return; fi
  done
  ip -4 route show default 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "dev") { print $(i + 1); exit }}'
}
i=0
DEV=$(pick)
while [ -z "$DEV" ] && [ "$i" -lt 20 ]; do sleep 1; i=$((i + 1)); DEV=$(pick); done
[ -n "$DEV" ] || exit 0
sleep 2   # give NetworkManager a moment to apply its own copy first
ip -4 addr show | grep -q "inet $ADDR/" || ip addr add "$ADDR/16" dev "$DEV" scope link
exit 0
