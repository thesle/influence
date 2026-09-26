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
port, the data directory, and whether TLS is enforced. When a required setting is
missing or invalid, the server fails fast at startup rather than running in a
degraded state.
