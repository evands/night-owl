#!/bin/sh
# Update a server installed with install.sh, without administrator rights:
#     sh update.sh <path to the new cdjhue program>
# Run as the user the service runs as. The lights pause for a few seconds.
set -e
NEW=${1:?usage: sh update.sh <new cdjhue program>}
cp "$NEW" /opt/cdjhue/app/cdjhue.new
chmod 755 /opt/cdjhue/app/cdjhue.new
mv /opt/cdjhue/app/cdjhue.new /opt/cdjhue/app/cdjhue
pkill -TERM -x cdjhue || true      # systemd starts the new one
echo "updated; the service restarts in about five seconds"
