#!/bin/sh
# postinstall: create/own the Data_Directory, reload systemd, and enable the
# service. Runs on both Debian/Ubuntu (dpkg) and Fedora/RHEL (rpm). Idempotent.
set -e

SERVICE_USER=influence
SERVICE_GROUP=influence
DATA_DIR=/var/lib/influence

# Create the Data_Directory with restrictive ownership/permissions if absent,
# and always ensure it is owned by the service user (mode 0750).
if [ ! -d "$DATA_DIR" ]; then
    mkdir -p "$DATA_DIR"
fi
chown "$SERVICE_USER":"$SERVICE_GROUP" "$DATA_DIR"
chmod 0750 "$DATA_DIR"

# Register the unit with systemd and enable it (do not force-start on install).
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload >/dev/null 2>&1 || true
    systemctl enable influence.service >/dev/null 2>&1 || true
fi

exit 0
