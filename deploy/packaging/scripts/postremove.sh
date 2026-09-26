#!/bin/sh
# postremove: reload systemd after the unit file is gone. Runs on both
# Debian/Ubuntu (dpkg) and Fedora/RHEL (rpm). Idempotent.
#
# The Data_Directory (/var/lib/influence) and the service user are intentionally
# left in place so a reinstall preserves tenant data, matching the Makefile
# `uninstall` behavior (Requirement 28.6). Operators who want a full purge can
# remove /var/lib/influence and the `influence` user manually.
set -e

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload >/dev/null 2>&1 || true
fi

exit 0
