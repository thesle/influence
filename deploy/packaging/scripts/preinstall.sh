#!/bin/sh
# preinstall: ensure the dedicated non-login `influence` service user and group
# exist before files are laid down. Runs on both Debian/Ubuntu (dpkg) and
# Fedora/RHEL (rpm). Idempotent: does nothing if the user/group already exist.
set -e

SERVICE_USER=influence
SERVICE_GROUP=influence
DATA_DIR=/var/lib/influence

if ! getent group "$SERVICE_GROUP" >/dev/null 2>&1; then
    groupadd --system "$SERVICE_GROUP"
fi

if ! getent passwd "$SERVICE_USER" >/dev/null 2>&1; then
    useradd --system \
        --gid "$SERVICE_GROUP" \
        --home-dir "$DATA_DIR" \
        --no-create-home \
        --shell /usr/sbin/nologin \
        "$SERVICE_USER"
fi

exit 0
