#!/bin/sh
# preremove: stop and disable the service before its files are removed. Runs on
# both Debian/Ubuntu (dpkg) and Fedora/RHEL (rpm). Idempotent.
#
# On dpkg the first argument is "remove"/"upgrade"; on rpm it is a numeric count
# of remaining installs ("0" on final removal). We stop/disable in both flows;
# systemctl is a no-op if the unit is already stopped.
set -e

if command -v systemctl >/dev/null 2>&1; then
    systemctl disable --now influence.service >/dev/null 2>&1 || true
fi

exit 0
