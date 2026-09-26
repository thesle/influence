# Administration

This guide covers the administrative tasks that keep an Influence deployment
running.

## Tenants

Influence is multitenant: each tenant is fully isolated in its own SQLite
database, and a record in one tenant is never visible from another. An
administrator can provision a new tenant, which creates the tenant database and
its initial key material in a single transaction.

## Users and groups

Access is controlled through groups rather than individual users:

- A **user** belongs to one or more **groups**.
- A **group** may be marked as an admin group, which grants administrative
  privileges to its members.
- **Sphere grants** connect a group to a Sphere with a level of access (read or
  write) and, optionally, reveal permission for encrypted spans.

Because permissions attach to groups, adding or removing a person's access is a
matter of adjusting their group membership.

## Backups and export

Each tenant's data lives in a single SQLite file under the data directory, so a
file-level backup of that directory captures everything. An administrator can
also export a consistent snapshot of an individual tenant on demand.

## Configuration

The server reads its settings, in order of precedence, from command-line flags,
environment variables, and a configuration file. Key settings include the listen
port, the data directory, and whether TLS is enforced.

### Accepted parameters

Each setting can be supplied as a config-file key, a command-line flag, or an
environment variable. The effective value is resolved with the fixed precedence
**CLI flag > environment variable > config file > built-in default**.

| Config-file key | CLI flag     | Environment variable      | Default                 | Description                                                                                                                          |
| --------------- | ------------ | ------------------------- | ----------------------- | ------------------------------------------------------------------------------------------------------------------------------------ |
| `port`          | `--port`     | `INFLUENCE_PORT`          | `8080`                  | TCP port the server listens on. Must be an integer in the range 1-65535.                                                             |
| `data_dir`      | `--data-dir` | `INFLUENCE_DATA_DIR`      | `/var/lib/influence`    | Directory holding the Central_Directory and every Tenant_Database. Must exist and be writable by the service user.                   |
| `tls`           | `--tls`      | `INFLUENCE_TLS`           | `false`                 | Enables in-process TLS termination. When `true`, `tls_cert` and `tls_key` are required.                                              |
| `tls_cert`      | `--tls-cert` | `INFLUENCE_TLS_CERT`      | _(none)_                | Path to the PEM-encoded TLS certificate. Required when `tls` is enabled.                                                             |
| `tls_key`       | `--tls-key`  | `INFLUENCE_TLS_KEY`       | _(none)_                | Path to the PEM-encoded TLS private key. Required when `tls` is enabled.                                                             |
| `master_secret` | _(none)_     | `INFLUENCE_MASTER_SECRET` | _(none)_                | Server master secret used to derive per-tenant key-encryption keys. Not settable via a flag so it never appears in the process list. |
| _(none)_        | `--config`   | `INFLUENCE_CONFIG`        | `/etc/influence/config` | Path to the configuration file. The file cannot set its own path, so this setting is flag/env only.                                  |

### Examples

The configuration file at `/etc/influence/config` uses simple `key = value`
lines; blank lines and lines beginning with `#` are ignored:

```ini
port = 8443
data_dir = /var/lib/influence
tls = true
tls_cert = /etc/influence/tls/cert.pem
tls_key  = /etc/influence/tls/key.pem
master_secret = replace-with-a-strong-random-value
```

Command-line flags accept the `--flag value`, `--flag=value`, and boolean
`--tls` / `--tls=false` forms (single-dash `-flag` is also accepted):

```sh
influence --port 8443 --data-dir /srv/influence --tls \
  --tls-cert /etc/influence/tls/cert.pem \
  --tls-key /etc/influence/tls/key.pem
```

Environment variables are convenient for secrets and for systemd or container
deployments:

```sh
export INFLUENCE_PORT=8443
export INFLUENCE_TLS=true
export INFLUENCE_TLS_CERT=/etc/influence/tls/cert.pem
export INFLUENCE_TLS_KEY=/etc/influence/tls/key.pem
export INFLUENCE_MASTER_SECRET=replace-with-a-strong-random-value
```

Because the master secret should never appear in a shell history or the process
list, prefer supplying it through the master-secret environment variable (or the
config file with restricted permissions) rather than on the command line.

When a required setting is missing or invalid - an out-of-range port, a
non-writable data directory, or TLS enabled without a certificate and key - the
server fails fast at startup rather than running in a degraded state.
