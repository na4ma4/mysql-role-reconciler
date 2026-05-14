# AGENTS.md

## Project Overview

MySQL Role Reconciler compares a declarative configuration of MySQL roles and permissions against the actual state across multiple servers, generates SQL migration statements, and can apply them while tracking history. Written in Go, targeting MySQL 8.0+ (native roles).

## Build & Test

```sh
go build ./...
go test ./internal/...
go vet ./...
go mod tidy
```

Mage is also available: `mage` runs format, tidy, lint, and test (default target via `github.com/dosquad/mage`).

## Project Structure

```
cmd/mysql-reconciler/     CLI entry point (cobra commands)
internal/config/          Config loading, validation, program merging, program filtering, error classification
internal/mysql/            MySQL connections (password + IAM auth), actual state reader
internal/reconcile/        Desired state builder, diff engine, SQL generation, database pattern expansion
internal/migrate/          Plan file I/O, history tracking, state store, storage abstraction (local + S3)
```

## CLI Commands

| Command | Signature | Hidden |
|---------|-----------|--------|
| `plan` | `plan -c config.yaml -e ENV [-p PROGRAM]... PLAN_FILE` | No |
| `apply` | `apply -c config.yaml [-e ENV] PLAN_FILE` | No |
| `show` | `show -c config.yaml [--drift] PLAN_FILE` | No |
| `history` | `history -c config.yaml [--last]` | No |
| `sort-plan` | `sort-plan PLAN_FILE` | No |
| `render` | `render -c config.yaml` | Yes |

Global flags: `-c/--config` (required), `-d/--debug`, `--save-dir` (defaults to `.`; overrides `state.dir` in config).

### Plan Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--config` | `-c` | Path to config.yaml (required) |
| `--environment` | `-e` | Environment name (required) |
| `--program` | `-p` | Target only the specified program(s); may be set multiple times |
| `--drop-roles` | | Include DROP ROLE statements for roles not in config |

When `--program` is specified, only the named program(s) are included in the plan. Their servers, schemas, and templated roles are scoped accordingly. Non-templated (global) roles naturally narrow to only the filtered programs' schemas. When omitted, all programs are included.

### Apply Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--config` | `-c` | Path to config.yaml (required) |
| `--environment` | `-e` | Environment name (defaults to the environment stored in the plan file) |

When `--environment` is omitted, the environment is read from the plan file. If specified, it must match the plan file's environment or an error is returned.

**Interrupt handling:** First Ctrl+C sets the interrupted flag, finishes the current statement, and saves partial state/history (marked stale). Second Ctrl+C forces immediate exit (`os.Exit(130)`). A stale state changes the checksum, forcing a re-plan before the next apply.

**Stale plan detection:** Plan files include `state_checksum` per server from the state store at plan time. On `apply`, `validatePlanState` checks the current state store checksum matches; mismatch means the plan is stale and must be regenerated.

### Show Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--config` | `-c` | Path to config.yaml (required) |
| `--drift` | | Show config drift and server drift against state store |
| `--output` | `-o` | Output format: text (default), markdown/md, json, yaml |

Markdown output groups statements by program within each server using collapsible `<details>` sections.

### History Flags

| Flag | Description |
|------|-------------|
| `--config` | `-c` | Path to config.yaml (required) |
| `--last` | Show only the most recent migration entry |

### sort-plan Command

Reads a plan file, re-sorts servers, statements, and grants into deterministic order, and rewrites in-place. Semantic content (SQL, checksum) is unchanged.

### render Command (Hidden)

Reads config, resolves all `!include` directives, and outputs fully-resolved YAML to stdout. Useful for debugging include tag expansion.

## Configuration

Three YAML files, referenced from `config.yaml`:

- **config.yaml** — `programs_file`, `servers_file`, `servers` (inline), `programs` (merged into programs file), `permission_sets`, `roles`, `state`
- **programs.yaml** — Programs with `server` (env→server mapping), `app_db`, `sup_db`, `ignore_errors`, `enabled`. Supports list and map formats.
- **servers.yaml** — Map of server name to connection config (`host`, `port`, `user`, `password`, `iam_auth`, `aws_region`, `aws_id`, `ssl`, `enabled`, `open_connections`, `idle_connections`, `max_conn_lifetime`)

The `programs:` list in config.yaml merges into the programs file by name. All fields are merged: maps are unioned, slices are appended with dedup. Config-level `ignore_errors` overrides file-level if set. Config-level `enabled` overrides file-level if explicitly set.

Config files support `!include` tags via `github.com/na4ma4/go-yamladv` for splitting configuration across files. Included paths are resolved relative to the config file's directory.

SSL paths (`ssl.ca`, `ssl.cert`, `ssl.key`) are resolved relative to the config file's directory. Absolute paths are kept as-is.

### Programs YAML Formats

**List format** (original):
```yaml
- name: myapp
  server: { prod: hyperion }
  app_db: [zban_myapp]
```

**Map format** (key is the program name):
```yaml
myapp:
  server: { prod: hyperion }
  app_db: [zban_myapp]
```

Both formats are parsed by `ProgramsFile.UnmarshalYAML`, which tries list first, then map.

### State Configuration

The `state:` block in config.yaml controls where state and history are persisted:

```yaml
state:
  storage: local     # "local" (default) or "s3"
  dir: "."           # base directory for local storage (default: current directory)
  # s3:              # S3 configuration (used when storage=s3)
  #   bucket: "my-state-bucket"
  #   prefix: "mysql-reconciler/"
  #   region: "us-east-1"
```

`--save-dir` flag overrides `state.dir`. S3 storage uses the AWS SDK Go v2 credential chain.

## Server Enabled Property

Servers and programs support an `enabled` property (default: `true`). When `enabled: false`, the server/program is skipped during `plan` and `apply` with a message. Omitting `enabled` is equivalent to `enabled: true`.

```yaml
# Active server (enabled defaults to true)
hyperion:
  host: "hyperion.example.com"
  port: 3306

# Disabled server
otherserver:
  host: "otherserver.example.com"
  enabled: false

# Disabled program
myapp:
  server: { prod: hyperion }
  app_db: [zban_myapp]
  enabled: false
```

Implementation uses `ptrVal[T]` to detect whether the `enabled` key was explicitly set; when absent, `setDefaults()` (servers) or `SetProgramDefaults` (programs) defaults it to `true`. `FilterDisabledPrograms` removes disabled programs after merge.

### ptrVal[T] — Omit-vs-Zero Detection

```go
type ptrVal[T any] struct { val *T }
```

Methods: `Set(v T)`, `Get() T` (zero value if unset), `IsSet() bool`. Used for `ServerConfig.Enabled`, `ServerConfig.OpenConnections`, `ServerConfig.IdleConnections`, `ServerConfig.MaxConnLifetime`, and `ProgramConfig.Enabled`. Enables distinguishing "key omitted in YAML" from "key explicitly set to zero value".

## Server Connection Pool

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `open_connections` | `ptrVal[int]` | `5` | Max open database connections; also used as concurrency semaphore for grant queries |
| `idle_connections` | `ptrVal[int]` | `5` | Max idle database connections |
| `max_conn_lifetime` | `ptrVal[time.Duration]` | `5m` | Max connection lifetime as Go duration string |

## Core Data Flow

1. **Load & merge** config → programs → servers → validate
2. **Filter programs** (if `--program` specified) → only target programs proceed
3. **Filter disabled** programs and servers (with `enabled: false`) → skipped with message
4. **Connect** to each enabled server (password auth or AWS IAM auth via `aws-sdk-go-v2`)
5. **Read actual state**: query `mysql.user` for role existence, `SHOW GRANTS FOR role@%`, parse grant strings
6. **Query database list**: `SELECT SCHEMA_NAME FROM information_schema.SCHEMATA` for pattern expansion
7. **Expand role templates**: replace `{{name}}` in role names with each program name; scope each expanded role's grants to that program's databases only
8. **Build desired state**: for each expanded role, resolve permission sets per scope (server `*.*`, app_db, sup_db) with table-level targeting
9. **Expand database patterns**: replace LIKE-pattern database names (containing `%`) with concrete matching databases from the server
10. **Diff**: compare desired vs actual → produce `MigrationStatement` list (create_role, grant, revoke, drop_role)
11. **Plan file**: JSON format with metadata, per-server statements, roles, grants, SHA256 checksum, and state checksum
12. **Apply**: execute SQL, write history entry, update state store with full desired state

## Key Types

| Package | Type | Purpose |
|---------|------|---------|
| `config` | `Config` | Top-level config (permission_sets, roles, file references, state config) |
| `config` | `ProgramConfig` | Program with server env map, app_db, sup_db, ignore_errors, enabled |
| `config` | `RoleConfig` | Role with per-scope permission set references |
| `config` | `ServerConfig` | Connection details (host, port, IAM auth, SSL, enabled, pool settings) |
| `config` | `ProgramDBs` | Database lists (AppDBs, SupDBs) for a program on a server |
| `config` | `IgnoreErrorsConfig` | Error ignore rules (All bool, Errors []MySQLErrorCode) |
| `config` | `MySQLErrorCode` | Named string type for MySQL error classification (e.g., "table_not_found") |
| `config` | `StateConfig` | State storage config (storage type, dir, S3 settings) |
| `config` | `S3Config` | S3 bucket, prefix, region |
| `config` | `ptrVal[T]` | Generic pointer-wrapper for omit-vs-zero detection in YAML |
| `mysql` | `Grant` | Parsed MySQL grant (role, schema, table, privileges) |
| `mysql` | `ActualState` | Current roles and grants on a server |
| `reconcile` | `DesiredGrant` | A grant that should exist (role, schema, table, privileges) |
| `reconcile` | `DesiredState` | Full desired state for a server |
| `reconcile` | `MigrationStatement` | SQL to execute with type classification |
| `reconcile` | `GrantEntry` | Generic grant record for comparing any two grant sets |
| `reconcile` | `Plan` | Per-server plan with statements, checksum, roles, grants |
| `migrate` | `PlanFile` | JSON plan with metadata and per-server plans |
| `migrate` | `ServerState` | Last-applied state per server (roles, grants, stale flag) |
| `migrate` | `ServerPlan` | Per-server section in plan file (statements, state_checksum) |
| `migrate` | `HistoryEntry` | Migration history record (version, timestamp, statements, optional error) |
| `migrate` | `GrantEntry` | Stored grant entry in state store |
| `migrate` | `Storage` | Interface for reading/writing state and history files |
| `migrate` | `StateStore` | In-memory state store backed by Storage |

## Error Classification & Ignore

`config.ClassifyError(err error) MySQLErrorCode` maps MySQL driver errors to named codes:

| MySQL Code | `MySQLErrorCode` Constant |
|------------|--------------------------|
| 1045 | `MySQLErrorAccessDenied` ("access_denied") |
| 1049 | `MySQLErrorSchemaNotFound` ("schema_not_found") |
| 1050 | `MySQLErrorAlreadyExists` ("already_exists") |
| 1054 | `MySQLErrorColumnNotFound` ("column_not_found") |
| 1062 | `MySQLErrorDuplicateEntry` ("duplicate_entry") |
| 1146 | `MySQLErrorTableNotFound` ("table_not_found") |
| 1394 | `MySQLErrorRoleNotFound` ("role_not_found") |
| 1396 | `MySQLErrorDuplicateRole` ("duplicate_role") |

Unrecognized MySQL errors → `"mysql_<code>"`. Non-MySQL errors → `"unknown"`.

`IgnoreErrorsConfig` on `ProgramConfig` controls which errors to skip during apply:

```yaml
ignore_errors: true                          # ignore all errors
ignore_errors: "table_not_found"             # single error
ignore_errors: ["table_not_found", "role_not_found"]  # list of errors
```

`ShouldIgnore(errType MySQLErrorCode)` returns true if `All` is set or if `errType` matches an entry in `Errors`. The `"all"` string in the error list also sets `All=true`-equivalent behavior.

During apply, each statement's role is mapped to a program via `BuildRoleProgramMap`, and the program's `IgnoreErrorsConfig` determines whether to skip or fail on MySQL errors. Ignored errors are logged with `~ IGNORED` prefix.

## Schema vs Table Semantics

**Critical distinction**: In the program config, `app_db` and `sup_db` list **schema** (database) names. In the role config, `app_db` and `sup_db` keys are **table names** within those schemas.

This means a role like:
```yaml
app_db:
  '*': [ "select", "create_temp" ]
  'objects': [ "select", "dml" ]
```
Produces grants per program schema:
- `GRANT SELECT, CREATE TEMPORARY TABLES ON `schema`.* TO 'role'` (all tables)
- `GRANT SELECT, INSERT, UPDATE, DELETE ON `schema`.`objects` TO 'role'` (specific table)

## Grant Structure

| DesiredGrant fields | SQL output |
|---|---|
| Database="*", Table="" | `*.*` (server-level) |
| Database="myschema", Table="*" | `` `myschema`.* `` (all tables in schema) |
| Database="myschema", Table="mytable" | `` `myschema`.`mytable` `` (specific table) |

## Permission Matching Logic

Within a role's scope (`app_db` or `sup_db`), the keys are table names:

- `'*'` — all tables in the schema → `GRANT ... ON schema.*`
- Exact name like `'objects'` — specific table → `GRANT ... ON schema.objects`
- LIKE pattern like `'dst_%'` — table pattern → `GRANT ... ON schema.dst_%`

Permission set names (e.g., `select`, `dml`) are resolved via the `permission_sets` map to actual MySQL privilege names (e.g., `SELECT`, `INSERT`, `UPDATE`, `DELETE`). Unrecognized names are treated as raw privilege names.

## Role Template Expansion

Role names containing `{{name}}` are templates expanded per program. The template placeholder is replaced with each program's name, and the expanded role's grants are scoped to that program's databases only.

- `tp-{{name}}-adm` for program `myapp` → role `tp-myapp-adm`, grants on `myapp`'s `app_db` and `sup_db` only
- Non-templated roles (no `{{name}}`) are global: their grants apply across all programs' databases on the server
- Template and global roles can be mixed in the same config

Key functions:
- `config.IsTemplate(name)` — checks if a role name contains `{{name}}`
- `config.ExpandRole(role, programName)` — substitutes `{{name}}` with the program name
- `config.ExpandRolesForServer(roles, progs, server, env)` — expands all templates for programs on a given server
- `config.FilterPrograms(progs, allowlist)` — filters programs to only those named in the allowlist; empty allowlist returns all
- `config.BuildRoleProgramMap(roles, programDBs)` — maps expanded role names to program names for O(1) lookup
- `config.FilterDisabledPrograms(progs)` — removes programs with `enabled: false`
- `reconcile.findProgramForRole(expandedName, templates, programDBs)` — reverse lookup to determine which program an expanded role belongs to

Template role names are not checked for duplicates during validation (they become unique after expansion). Duplicate checking of concrete role names happens implicitly in `ExpandRolesForServer` via a `seen` map.

## Database Pattern Expansion

Database names in program config containing `%` are treated as LIKE patterns. During `plan`, `QueryDatabases` queries `information_schema.SCHEMATA` to get the server's database list, then `ExpandDatabasePatterns` expands pattern grants into concrete per-database grants.

- `IsDatabasePattern(db)` returns true if `db` contains `%`
- Only `%` is treated as a wildcard in database names; `_` is matched literally (too common in DB names)
- `likeMatchDB` implements this restricted LIKE matching
- Example: `app_db: ["zban_qa%"]` expands to all databases matching `zban_qa%` on that server

## Grant String Parsing

`parseGrantString` → `parseTargetFromGrant` handles:
- `GRANT USAGE ON *.* TO 'role'@'%'` → Database="*", Table="" (server level)
- `GRANT SELECT ON `mydb`.* TO 'role'@'%'` → Database="mydb", Table="*" (schema level)
- `GRANT SELECT ON `mydb`.`objects` TO 'role'@'%'` → Database="mydb", Table="objects" (table level)
- `GRANT ALL PRIVILEGES ON *.* TO 'role'@'%' WITH GRANT OPTION` → single `ALL PRIVILEGES` privilege, grant option flag

## Diff Semantics

Three diffs are supported via `reconcile.DiffGrants`, which compares any two grant sets:

| Diff | From | To | Use case |
|------|------|----|----------|
| Desired vs Current | live server actual state | config desired state | Migration SQL (`plan`) |
| Desired vs Last-applied | last-applied desired state | config desired state | Config drift since last apply |
| Current vs Last-applied | last-applied desired state | live server actual state | Server drift since last apply |

Statement rules:
- Missing role → `CREATE ROLE`
- Missing privilege on existing role/schema/table → `GRANT perm1, perm2 ON target TO role` (combined)
- Extra privilege not in desired config → `REVOKE perm ON target FROM role`
- Grant target in "from" but not in "to" → `REVOKE all ON target FROM role`
- Role in "from" but not in "to" → `DROP ROLE` (only with `--drop-roles` flag)
- `ALL PRIVILEGES` in desired subsumes any extra actual privileges (no revoke generated)
- `USAGE` is a no-op sentinel: if desired has `USAGE` but actual already has a non-`USAGE` privilege on the target, `USAGE` grant is skipped (subsumed)
- `REVOKE USAGE` is never generated (it's always implicit in MySQL)
- `GRANT OPTION` is detected and tracked in parsed grants

`show --drift` displays both config drift and server drift against the state store. Reports stale state markers from partial applies.

## Plan File Format

JSON with this shape:
```json
{
  "generated_at": "2026-05-14T10:00:00Z",
  "environment": "prod",
  "servers": [
    {
      "server": "hyperion",
      "state_checksum": "sha256-hex",
      "roles": ["tp-myapp-adm", "tp-myapp-ro"],
      "grants": [
        {"role": "tp-myapp-adm", "database": "*", "table": "", "privileges": ["PROCESS"]},
        {"role": "tp-myapp-adm", "database": "zban_myapp", "table": "*", "privileges": ["SELECT", "INSERT"]}
      ],
      "statements": [
        {"sql": "CREATE ROLE 'tp-myapp-adm'", "type": "create_role", "role": "tp-myapp-adm"}
      ],
      "checksum": "sha256-hex"
    }
  ]
}
```

`state_checksum` captures the state store fingerprint at plan time. On `apply`, `validatePlanState` verifies it hasn't changed.

Statements are sorted for deterministic output: `create_role` → `grant` → `revoke` → `drop_role`, then by role, database, table within each type.

## State & History Storage

Backed by the `migrate.Storage` interface with two implementations:

- **`LocalStorage`** — filesystem-based; `ReadFile`/`WriteFile`/`ListFiles` with sorted results, excludes dotfiles
- **`S3Storage`** — AWS S3-based; same interface using `s3.GetObject`/`PutObject`/`ListObjectsV2Paginator`; prefix auto-appends `/`; requires non-empty bucket

### State Store

`.mysql-reconciler-state.json`: maps server name → `ServerState` including full roles list, grant entries, and stale marker.

```json
{
  "hyperion": {
    "applied_at": "2026-05-14T10:00:00Z",
    "environment": "prod",
    "checksum": "sha256-hex",
    "roles": ["tp-myapp-adm", "tp-myapp-ro"],
    "grants": [
      {"role": "tp-myapp-adm", "database": "*", "table": "", "privileges": ["PROCESS"]},
      {"role": "tp-myapp-adm", "database": "zban_myapp", "table": "*", "privileges": ["SELECT", "INSERT"]}
    ],
    "stale": false
  }
}
```

`ServerState` fields: `AppliedAt`, `Environment`, `Checksum`, `Roles`, `Grants`, `Stale` (omitempty), `StaleAt` (omitempty), `Error` (omitempty).

**Stale state:** When an apply fails or is interrupted, `MarkStale` sets `Stale=true`, `StaleAt`, and `Error`. This changes the state checksum, which forces a re-plan before the next apply. `ChecksumFor(server)` computes a deterministic SHA256 including stale fields.

### History

`.mysql-reconciler-history/`: one YAML file per apply.

- **v2 format** (current): UUIDv7 filenames (chronological sort), proper YAML with `version: v2`
- **v1 format** (legacy): sequence-number filenames, also readable

`HistoryEntry` fields: `Version`, `Timestamp`, `Environment`, `Server`, `Statements`, `Checksum`, `Error` (omitempty), `FailedSQL` (omitempty).

Partial apply entries record error details and which SQL failed.

## Dependencies

- `github.com/spf13/cobra` — CLI framework
- `github.com/spf13/viper` — flag/env binding
- `github.com/go-sql-driver/mysql` — MySQL driver (TLS config registration)
- `github.com/aws/aws-sdk-go-v2/config` + `feature/rds/auth` — IAM auth token generation
- `github.com/aws/aws-sdk-go-v2/service/s3` — S3 storage backend
- `go.yaml.in/yaml/v3` — YAML parsing
- `github.com/na4ma4/go-yamladv` — `!include` tag resolution for YAML files
- `github.com/dosquad/go-cliversion` — version info

## Go Conventions

- Go 1.26.1, module path `github.com/na4ma4/mysql-role-reconciler`
- Internal packages only (`internal/`)
- Error wrapping with `fmt.Errorf("context: %w", err)`
- Table-driven tests in `*_test.go` files
- Map value mutability: when iterating `map[string]T`, re-assign after modifying: `m[key] = val`
- YAML defaulting: use `ptrVal[T]` to detect omitted keys vs explicit zero values (e.g., `enabled` defaulting to `true` via `setDefaults()`)
- Named string types for domain concepts: `MySQLErrorCode` for error classification
- `//nolint:lintername // explanation` pattern for justified suppressions
- Block scoping with `{ var err error; ... }` pattern for narrowing variable scope
