# Makefile for the Influence documentation platform.
#
# Operator targets (Requirement 28.1):
#   make dev        - run the server in the foreground (no systemd)
#   make build      - build a single FTS5-enabled static binary
#   make install    - install binary, service user, data dir, config, systemd unit
#   make uninstall  - stop/disable/remove the service and binary (data preserved)
#
# The server uses modernc.org/sqlite, a pure-Go SQLite driver with FTS5 support
# built in (see internal/data/sqlite.go). No cgo and no build tags are required
# for FTS5; a fully static binary is produced simply by disabling cgo.

# ---- Configuration -----------------------------------------------------------

# Go entrypoint package.
PKG        := ./cmd/influence

# Local build output (Requirement 28.3).
BIN_DIR    := bin
BIN        := $(BIN_DIR)/influence

# Install locations (match design § systemd unit / Makefile targets).
INSTALL_BIN   := /usr/local/bin/influence
SERVICE_USER  := influence
SERVICE_GROUP := influence
DATA_DIR      := /var/lib/influence
CONFIG_DIR    := /etc/influence
CONFIG_FILE   := $(CONFIG_DIR)/config

# systemd unit source (authored by task 25.2) and its installed location.
UNIT_SRC   := deploy/influence.service
UNIT_DST   := /etc/systemd/system/influence.service

# Dev run configuration. A throwaway data dir keeps `make dev` self-contained
# and avoids requiring the default /var/lib/influence to exist.
DEV_DATA_DIR := .dev/data
DEV_PORT     ?= 8080

# Static, FTS5-enabled build: pure-Go SQLite means CGO can stay off, yielding a
# single statically linked binary.
CGO_ENABLED  := 0
GO           ?= go

# Packaging (task 25.2). nfpm generates both .deb and .rpm from one manifest.
NFPM        ?= nfpm
NFPM_CONFIG := deploy/nfpm.yaml
DIST_DIR    := dist

.PHONY: dev build install uninstall clean package

# ---- dev (Requirement 28.2) --------------------------------------------------
# Run the server in the foreground so it listens for connections and exits when
# the operator interrupts it (Ctrl-C). Does NOT create or touch a systemd
# service. Uses a local dev data directory that must exist before startup
# (the server fails fast if the Data_Directory is missing).
dev:
	@mkdir -p $(DEV_DATA_DIR)
	$(GO) run $(PKG) --port $(DEV_PORT) --data-dir $(DEV_DATA_DIR) --config /dev/null

# ---- build (Requirement 28.3) ------------------------------------------------
# Produce a single FTS5-enabled, statically linked binary. On success the
# recipe exits 0; on any build failure `go build` exits non-zero and prints the
# compiler error, which make propagates as a non-success status.
build:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build -o $(BIN) $(PKG)
	@echo "built $(BIN)"

# ---- install (Requirements 28.4, 28.7) ---------------------------------------
# Idempotent: every step is guarded so a re-run produces the same end state and
# never errors on an already-present resource. Requires root (writes under
# /usr/local/bin, /etc, /var/lib and manages systemd).
install: build
	@echo "installing influence..."

	# Create the dedicated non-login system service user/group if absent.
	@if ! getent group $(SERVICE_GROUP) >/dev/null 2>&1; then \
		groupadd --system $(SERVICE_GROUP); \
		echo "created group $(SERVICE_GROUP)"; \
	else \
		echo "group $(SERVICE_GROUP) already exists"; \
	fi
	@if ! id -u $(SERVICE_USER) >/dev/null 2>&1; then \
		useradd --system --gid $(SERVICE_GROUP) --home-dir $(DATA_DIR) \
			--no-create-home --shell /usr/sbin/nologin $(SERVICE_USER); \
		echo "created user $(SERVICE_USER)"; \
	else \
		echo "user $(SERVICE_USER) already exists"; \
	fi

	# Install the binary (install(1) overwrites atomically and is idempotent).
	install -D -m 0755 $(BIN) $(INSTALL_BIN)
	@echo "installed binary to $(INSTALL_BIN)"

	# Create and take ownership of the Data_Directory.
	install -d -m 0750 -o $(SERVICE_USER) -g $(SERVICE_GROUP) $(DATA_DIR)
	@echo "ensured data dir $(DATA_DIR)"

	# Install a default config only if none already exists, preserving any
	# operator customizations across re-installs (Requirement 28.4).
	install -d -m 0755 $(CONFIG_DIR)
	@if [ ! -e $(CONFIG_FILE) ]; then \
		printf 'port = %s\ndata_dir = %s\ntls = false\n' "8080" "$(DATA_DIR)" > $(CONFIG_FILE); \
		chmod 0644 $(CONFIG_FILE); \
		echo "installed default config to $(CONFIG_FILE)"; \
	else \
		echo "config $(CONFIG_FILE) already exists; leaving untouched"; \
	fi

	# Install and enable the systemd unit (deploy/influence.service is authored
	# by task 25.2). enable --now is idempotent for an already-enabled unit.
	@if [ ! -f $(UNIT_SRC) ]; then \
		echo "error: $(UNIT_SRC) not found (created by task 25.2)" >&2; \
		exit 1; \
	fi
	install -D -m 0644 $(UNIT_SRC) $(UNIT_DST)
	systemctl daemon-reload
	systemctl enable influence.service
	@echo "install complete"

# ---- uninstall (Requirements 28.5, 28.6, 28.7) -------------------------------
# Idempotent: guarded so a re-run (or a run against a partial install) produces
# the same end state and never errors on an already-absent resource. Preserves
# the Data_Directory and its databases unless the operator sets PURGE_DATA=1.
uninstall:
	@echo "uninstalling influence..."

	# Stop + disable the service if the unit is known to systemd.
	@if systemctl list-unit-files influence.service >/dev/null 2>&1 && \
		systemctl list-unit-files influence.service 2>/dev/null | grep -q influence.service; then \
		systemctl disable --now influence.service >/dev/null 2>&1 || true; \
		echo "stopped and disabled influence.service"; \
	else \
		echo "influence.service not present; skipping stop/disable"; \
	fi

	# Remove the systemd unit and reload if it existed.
	@if [ -f $(UNIT_DST) ]; then \
		rm -f $(UNIT_DST); \
		systemctl daemon-reload; \
		echo "removed $(UNIT_DST)"; \
	else \
		echo "$(UNIT_DST) not present; skipping"; \
	fi

	# Remove the installed binary.
	@if [ -e $(INSTALL_BIN) ]; then \
		rm -f $(INSTALL_BIN); \
		echo "removed $(INSTALL_BIN)"; \
	else \
		echo "$(INSTALL_BIN) not present; skipping"; \
	fi

	# Preserve the Data_Directory unless PURGE_DATA=1 (Requirement 28.6).
	@if [ "$(PURGE_DATA)" = "1" ]; then \
		rm -rf $(DATA_DIR); \
		echo "purged data dir $(DATA_DIR) (PURGE_DATA=1)"; \
	else \
		echo "preserved data dir $(DATA_DIR) (set PURGE_DATA=1 to remove)"; \
	fi
	@echo "uninstall complete"

# ---- package (Requirements 20.1, 20.2, 28.4) ---------------------------------
# Build the binary, then produce a .deb (Ubuntu 20.04+) and a .rpm (Fedora 38+)
# from the single nfpm manifest under deploy/nfpm.yaml. Requires nfpm on PATH
# (https://nfpm.goreleaser.com). This target is independent of install/uninstall
# and does not touch the local system.
package: build
	@command -v $(NFPM) >/dev/null 2>&1 || { \
		echo "error: $(NFPM) not found on PATH (install from https://nfpm.goreleaser.com)" >&2; \
		exit 1; \
	}
	@mkdir -p $(DIST_DIR)
	$(NFPM) package --config $(NFPM_CONFIG) --packager deb --target $(DIST_DIR)/
	$(NFPM) package --config $(NFPM_CONFIG) --packager rpm --target $(DIST_DIR)/
	@echo "packages written to $(DIST_DIR)/"

# ---- clean -------------------------------------------------------------------
# Remove local build artifacts and the dev data directory.
clean:
	rm -rf $(BIN_DIR) .dev $(DIST_DIR)
