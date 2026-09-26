# Influence

Influence is a self-hostable, multitenant documentation platform for non-profits.
It is a single Go process that serves a SvelteKit web application, stores all data
in SQLite, and secures transport with TLS.

## What Influence is for

Influence organizes an organization's knowledge into a navigable hierarchy:

- **Spheres** group related material and are the unit of access control.
- **Facets** subdivide a Sphere into a tree of topics.
- **Polygons** are the documents themselves, authored in markdown.

Sensitive passages inside a Polygon can be encrypted into obfuscation tokens and
revealed only to users who hold reveal permission for the owning Sphere.

## Running Influence

Influence runs as a single standalone process on a Linux host. The `Makefile`
provides the common lifecycle targets:

| Target      | What it does                                                        |
| ----------- | ------------------------------------------------------------------- |
| `dev`       | Runs the server in the foreground for local development.            |
| `build`     | Produces a runnable server binary.                                  |
| `install`   | Installs the binary and a systemd service under a dedicated user.   |
| `uninstall` | Removes the installed binary and service.                           |

## Documentation

- [Getting Started](getting-started.md) — first run, logging in, creating content.
- [Administration](administration.md) — tenants, users, groups, and backups.

These files are plain markdown so they render both on GitHub and inside the
Influence web application.
