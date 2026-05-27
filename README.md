# mysql-role-reconciler

[![Unit Tests](https://github.com/na4ma4/mysql-role-reconciler/actions/workflows/unit-test.yml/badge.svg)](https://github.com/na4ma4/mysql-role-reconciler/actions/workflows/unit-test.yml)
[![Release](https://github.com/na4ma4/mysql-role-reconciler/actions/workflows/release-please.yml/badge.svg)](https://github.com/na4ma4/mysql-role-reconciler/actions/workflows/release-please.yml)
[![Go Version](https://img.shields.io/github/go-mod/go-version/na4ma4/mysql-role-reconciler)](https://github.com/na4ma4/mysql-role-reconciler/blob/main/go.mod)
[![Latest Release](https://img.shields.io/github/v/release/na4ma4/mysql-role-reconciler?sort=semver)](https://github.com/na4ma4/mysql-role-reconciler/releases)
[![Go Report Card](https://goreportcard.com/badge/github.com/na4ma4/mysql-role-reconciler)](https://goreportcard.com/report/github.com/na4ma4/mysql-role-reconciler)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)


MySQL Role Reconciler compares a declarative configuration of MySQL roles and permissions against the actual state across multiple servers, generates SQL migration statements, and can apply them while tracking history. Written in Go, targeting MySQL 8.0+ (native roles).

## Overview

The tool operates in two phases:

1. **`plan`** — Connects to each server, reads the current roles and grants, compares against the desired state from config, and writes a JSON plan file containing the migration SQL.
2. **`apply`** — Executes the migration statements from the plan file, records history, and updates the state store for future drift detection.

A third phase, **`show --drift`**, compares current and last-applied states to detect both config drift (changes since last apply) and server drift (out-of-band modifications).

## Install

```sh
go install github.com/na4ma4/mysql-role-reconciler/cmd/mysql-reconciler@latest
```

## Quick Start

```sh
# Generate a migration plan
mysql-reconciler plan -c config.yaml -e prod plan.json

# Review the plan
mysql-reconciler show -c config.yaml plan.json

# Apply the plan
mysql-reconciler apply -c config.yaml plan.json

# Check for drift since last apply
mysql-reconciler show -c config.yaml --drift plan.json
```

Example configs: [`config.example.yaml`](config.example.yaml), [`programs.example.yaml`](programs.example.yaml), [`servers.example.yaml`](servers.example.yaml).

## CLI Commands

| Command | Description |
|---------|-------------|
| `plan` | Generate migration plan (dry-run) |
| `apply` | Apply migration plan to servers |
| `show` | Show plan details and drift |
| `history` | Show migration history |
| `sort-plan` | Re-sort a plan file into deterministic order |

### Global Flags

| Flag | Env | Description |
|------|-----|-------------|
| `-c/--config` | `MYSQL_ROLE_RECONCILER_CONFIG_FILE` | Path to config.yaml (required) |
| `-d/--debug` | `DEBUG` | Enable debug output |
| `--save-dir` | `MYSQL_ROLE_RECONCILER_SAVE_DIR` | Override directory for state and history |

### `plan`

```
mysql-reconciler plan -c config.yaml -e ENV [-p PROGRAM]... PLAN_FILE
```

| Flag | Short | Description |
|------|-------|-------------|
| `--environment` | `-e` | Environment name (required) |
| `--program` | `-p` | Target only the specified program(s); may be set multiple times |
| `--drop-roles` | | Include DROP ROLE statements for roles not in config |

When `--program` is specified, only the named program(s) are included in the plan. Their servers, schemas, and templated roles are scoped accordingly. Non-templated (global) roles naturally narrow to only the filtered programs' schemas.

### `apply`

```
mysql-reconciler apply -c config.yaml [-e ENV] PLAN_FILE
```

| Flag | Short | Description |
|------|-------|-------------|
| `--environment` | `-e` | Environment name (defaults to the environment stored in the plan file) |

When `--environment` is omitted, the environment is read from the plan file. If specified, it must match the plan file's environment or an error is returned.

**Interrupt handling:** The first Ctrl+C finishes the current statement and saves partial state/history; the second Ctrl+C forces an immediate exit. After a partial apply, the state is marked stale, which forces a re-plan before the next apply.

### `show`

```
mysql-reconciler show -c config.yaml [--drift] PLAN_FILE
```

| Flag | Short | Description |
|------|-------|-------------|
| `--drift` | | Show config drift and server drift against state store |
| `--output` | `-o` | Output format: `text` (default), `markdown`, `json`, `yaml` |

### `history`

```
mysql-reconciler history -c config.yaml [--last]
```

| Flag | Description |
|------|-------------|
| `--last` | Show only the most recent migration entry |

### `sort-plan`

```
mysql-reconciler sort-plan PLAN_FILE
```

Re-sorts servers, statements, and grants in the plan file into deterministic order in-place. Semantic content (SQL, checksum) is unchanged.

## Configuration

Three YAML files, referenced from `config.yaml`:

### config.yaml

```yaml
programs_file: "programs.yaml"     # optional: path to programs file
servers_file: "servers.yaml"       # optional: path to servers file

# Inline servers/programs (used when separate files are not specified)
servers: { ... }
programs: [ ... ]                  # merged into programs from programs_file

permission_sets:
  select: [SELECT]
  dml: [INSERT, UPDATE, DELETE]
  create_temp: [CREATE TEMPORARY TABLES]
  ddl: [CREATE, ALTER, DROP]
  all: [ALL PRIVILEGES]
  process: [PROCESS]

roles:
  - name: "{{name}}-adm"
    server:
      '*': ["process"]
    app_db:
      '*': ["select", "create_temp", "dml"]
    sup_db:
      '*': ["select", "create_temp", "dml", "ddl"]

# State storage configuration
state:
  storage: local     # "local" (default) or "s3"
  dir: "."           # base directory for local storage
  # s3:              # S3 configuration (used when storage=s3)
  #   bucket: "my-state-bucket"
  #   prefix: "mysql-reconciler/"
  #   region: "us-east-1"
```

Config files support `!include` tags for splitting configuration across files.

### programs.yaml

Supports two formats — **list** (original):

```yaml
- name: myapp
  server: { prod: rdsserver1, sham: rdsserver1-sham }
  app_db: [appname_myapp]
  sup_db: [myapp]
```

Or **map** (key is the program name):

```yaml
myapp:
  server: { prod: rdsserver1 }
  app_db: [appname_myapp]
  sup_db: [myapp]
```

| Field | Description |
|-------|-------------|
| `name` | Program name (required in list format; key serves as name in map format) |
| `server` | Map of environment name → server name |
| `app_db` | List of application database (schema) names |
| `sup_db` | List of support database (schema) names |
| `enabled` | Whether the program is active (default: `true`) |
| `ignore_errors` | Which MySQL errors to ignore during apply |

The `programs:` list in config.yaml merges into the programs file by name. All fields are merged: maps are unioned, slices are appended with dedup. Config-level `ignore_errors` and `enabled` override file-level values when set.

### servers.yaml

```yaml
rdsserver1:
  host: "rdsserver1.example.com"
  port: 3306
  user: "reconciler"
  password: "secret"
  ssl:
    ca: "/path/to/ca.pem"
    cert: "/path/to/cert.pem"
    key: "/path/to/key.pem"

rdsserver1-sham:
  host: "rdsserver1-sham.example.com"
  port: 3306
  user: "reconciler"
  aws_region: "us-east-1"
  iam_auth: true

otherserver:
  host: "otherserver.example.com"
  enabled: false
```

| Field | Description |
|-------|-------------|
| `host` | MySQL server hostname (required) |
| `port` | MySQL port (default: `3306`) |
| `user` | MySQL user (required) |
| `password` | MySQL password (for password auth) |
| `iam_auth` | Use AWS IAM authentication (default: `false`) |
| `aws_region` | AWS region for IAM auth token generation |
| `aws_id` | AWS identifier for IAM auth endpoint |
| `enabled` | Whether the server is active (default: `true`) |
| `ssl.ca` | Path to CA certificate file |
| `ssl.cert` | Path to client certificate file |
| `ssl.key` | Path to client key file |
| `open_connections` | Max open connections (default: `5`) |
| `idle_connections` | Max idle connections (default: `5`) |
| `max_conn_lifetime` | Max connection lifetime, Go duration (default: `5m`) |

SSL paths are resolved relative to the config file's directory. Absolute paths are kept as-is. When no SSL config is provided, connections still use TLS (encrypted without verification).

## Role Templates

Role names containing `{{name}}` are templates expanded per program:

- `{{name}}-adm` for program `myapp` → role `myapp-adm`, with grants scoped to `myapp`'s databases only
- Non-templated roles (no `{{name}}`) are global: their grants apply across all programs' databases on the server

## Schema vs Table Semantics

In the program config, `app_db` and `sup_db` list **schema** (database) names. In the role config, `app_db` and `sup_db` keys are **table names** within those schemas.

```yaml
# Role config — keys are table names within each program's schemas
app_db:
  '*': ["select", "create_temp"]          # all tables → GRANT ... ON schema.*
  'objects': ["select", "dml"]            # specific table → GRANT ... ON schema.objects
  'src_%': ["select", "dml", "ddl"]       # LIKE pattern → GRANT ... ON schema.src_%
```

Server-scope keys in the `server` field:

| Key | SQL output |
|-----|-----------|
| `'*'` | `*.*` (server-level) |
| `'dbname.*'` | `` `dbname`.* `` (all tables in schema) |
| `'dbname.tablename'` | `` `dbname`.`tablename` `` (specific table) |

## Database Pattern Expansion

Database names containing `%` in program config are treated as LIKE patterns. During `plan`, the tool queries `information_schema.SCHEMATA` to find matching databases and expands pattern grants into concrete per-database grants.

```yaml
app_db: ["appname_qa%"]   # expands to all databases matching appname_qa% on that server
```

Only `%` is treated as a wildcard; `_` is matched literally.

## Error Handling

Programs can configure which MySQL errors to ignore during apply:

```yaml
# Ignore all errors
ignore_errors: true

# Ignore specific errors
ignore_errors: ["table_not_found", "role_not_found"]

# Single error
ignore_errors: "table_not_found"
```

Named error types:

| Code | Name |
|------|------|
| 1045 | `access_denied` |
| 1049 | `schema_not_found` |
| 1050 | `already_exists` |
| 1054 | `column_not_found` |
| 1062 | `duplicate_entry` |
| 1146 | `table_not_found` |
| 1394 | `role_not_found` |
| 1396 | `duplicate_role` |

Unrecognized MySQL errors are classified as `mysql_<code>`. Non-MySQL errors are classified as `"unknown"`. Ignored errors are logged with `~ IGNORED` during apply.

## Drift Detection

Three-way diff support via `show --drift`:

| Diff | From | To | Purpose |
|------|------|----|---------|
| Migration | Live server | Config desired state | Plan SQL statements |
| Config drift | Last-applied state | Config desired state | Changes since last apply |
| Server drift | Last-applied state | Live server | Out-of-band modifications |

## Statement Ordering

Statements are sorted for deterministic output:

1. `create_role`
2. `grant`
3. `revoke`
4. `drop_role`

Within each type, sorted by role, database, table.

## State & History

**State store** (`.mysql-reconciler-state.json`): maps server name → last-applied desired state (roles, grants, checksum). Used for drift detection and stale plan validation.

**History** (`.mysql-reconciler-history/`): one YAML file per apply, containing timestamp, environment, server, statements, and checksum. Partial applies also record error details and the failed SQL.

**Stale state:** When an apply fails or is interrupted, the state is marked stale. This changes the state checksum, which forces a re-plan before the next apply. The `apply` command also validates that the plan file's state checksum matches the current state store.

Both are stored relative to `--save-dir` (default: current directory) or the `state.dir` config option. S3 storage is also available via `state.storage: s3`.

## Authentication

- **Password auth** — `password` field in server config
- **AWS IAM auth** — set `iam_auth: true` with `aws_region`; generates short-lived auth tokens via `aws-sdk-go-v2`
- **TLS/SSL** — configure `ssl.ca` (server verification), `ssl.cert` + `ssl.key` (mutual TLS); encrypted connections are used by default

## Build & Test

```sh
go build ./...
go test ./internal/...
go vet ./...
go mod tidy
```

Mage is also available: `mage` runs format, tidy, lint, and test.

## License

[MIT](LICENSE)
