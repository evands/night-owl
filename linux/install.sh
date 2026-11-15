#!/bin/sh
# Install cdjhue as a service that starts at boot. Run as root (sudo) from
# the folder holding cdjhue, cdjhue.service, link-local.sh and README.md:
#     sudo sh install.sh <user to run as> [hue bridge address]
# The bridge address is only used for the first pairing: with it, the
# service waits for the button on that bridge instead of on any bridge.
set -e
RUNAS=${1:?usage: sudo sh install.sh <user> [hue bridge address]}
BRIDGE=${2:+-bridge $2 }
HOMEDIR=$(getent passwd "$RUNAS" | cut -d: -f6)
[ -n "$HOMEDIR" ] || { echo "no such user: $RUNAS"; exit 1; }

# /opt/cdjhue holds the helper that runs as root, so it stays root-owned.
# The program itself goes in /opt/cdjhue/app, owned by the service user:
# that user can then update it (copy the new file in, stop the old one, and
# systemd starts the new one) without the administrator password.
install -d /opt/cdjhue
install -d -o "$RUNAS" /opt/cdjhue/app
systemctl stop cdjhue 2>/dev/null || true
install -m 755 link-local.sh /opt/cdjhue/
install -m 755 -o "$RUNAS" cdjhue /opt/cdjhue/app/
rm -f /opt/cdjhue/cdjhue
install -m 644 README.md /opt/cdjhue/ 2>/dev/null || true
sed -e "s|CDJHUE_USER|$RUNAS|" -e "s|CDJHUE_HOME|$HOMEDIR|" -e "s|CDJHUE_BRIDGE|$BRIDGE|" cdjhue.service > /etc/systemd/system/cdjhue.service

# Decks on self-assigned addresses are only heard with a 169.254 address
# of our own. Ask NetworkManager to keep one on the wired connection if
# there is one (Wi-Fi delays the decks' packets), else on the main
# connection, and take it off any other connection. link-local.sh adds it
# at service start as a backstop.
ADDR=169.254.57.59/16
if command -v nmcli >/dev/null 2>&1; then
  DEV=$(nmcli -t -f DEVICE,TYPE,STATE device status 2>/dev/null | awk -F: '$2 == "ethernet" && $3 == "connected" { print $1; exit }')
  [ -n "$DEV" ] || DEV=$(ip -4 route show default 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "dev") { print $(i + 1); exit }}')
  CON=$(nmcli -g GENERAL.CONNECTION device show "$DEV" 2>/dev/null || true)
  if [ -n "$CON" ]; then
    nmcli -t -f NAME,DEVICE connection show | while IFS=: read -r name dev; do
      [ "$name" = "$CON" ] && continue
      if nmcli -g ipv4.addresses connection show "$name" 2>/dev/null | grep -q "${ADDR%/*}"; then
        nmcli connection modify "$name" -ipv4.addresses "$ADDR" && echo "network: removed ${ADDR%/*} from '$name'"
        [ -n "$dev" ] && { nmcli device reapply "$dev" >/dev/null 2>&1 || true; }
      fi
    done
    if ! nmcli -g ipv4.addresses connection show "$CON" 2>/dev/null | grep -q "${ADDR%/*}"; then
      nmcli connection modify "$CON" +ipv4.addresses "$ADDR" && echo "network: '$CON' ($DEV) now also keeps ${ADDR%/*}"
      nmcli device reapply "$DEV" >/dev/null 2>&1 || true
    fi
  fi
fi

# Open the control page and the decks' ports if a firewall is running.
if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q "Status: active"; then
  ufw allow 8600/tcp comment "Night Owl control page" >/dev/null
  ufw allow 50000:50002/udp comment "Night Owl: Pioneer DJ link" >/dev/null
  echo "firewall: opened 8600/tcp and 50000-50002/udp"
fi

systemctl daemon-reload
systemctl enable --now cdjhue
sleep 3
systemctl --no-pager --lines=12 status cdjhue || true
echo
echo "Installed. It starts at boot. Logs: journalctl -u cdjhue -f"
