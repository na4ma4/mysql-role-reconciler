//nolint:goconst // meaningful strings.
package reconcile

import (
	"fmt"
	"sort"
	"strings"

	"github.com/na4ma4/mysql-role-reconciler/internal/config"
	"github.com/na4ma4/mysql-role-reconciler/internal/mysql"
)

// DesiredGrant represents a single grant that should exist.
type DesiredGrant struct {
	Role       string
	Database   string // schema name; "*" = server-level (*.*)
	Table      string // table name; "*" = all tables in schema (schema.*)
	Privileges []string
}

// DesiredState is the set of grants that should exist for a given server.
type DesiredState struct {
	Server string
	Grants []DesiredGrant
	Roles  []string
}

type StatementType string

// String returns a human-friendly name for the statement type, e.g., "GRANT" for StatementGrant.
func (s StatementType) String() string {
	return string(s)
}

// CompareOrder returns an integer representing the relative order of this statement type
// for sorting purposes. Lower numbers should be executed first.
//
//nolint:mnd // This is not a magic number, it's just the defined order of statement types.
func (s StatementType) CompareOrder() int {
	switch s {
	case StatementCreateRole:
		return 0
	case StatementGrant:
		return 1
	case StatementRevoke:
		return 2
	case StatementDropRole:
		return 3
	default:
		return 4
	}
}

const (
	StatementCreateRole StatementType = "create_role"
	StatementGrant      StatementType = "grant"
	StatementRevoke     StatementType = "revoke"
	StatementDropRole   StatementType = "drop_role"
)

// MigrationStatement represents a SQL statement to be executed.
type MigrationStatement struct {
	SQL      string        `json:"sql"`
	Type     StatementType `json:"type"` // "create_role", "grant", "revoke", "drop_role"
	Role     string        `json:"role"`
	Database string        `json:"database"`
	Table    string        `json:"table"`
}

// Plan represents a migration plan for a server.
type Plan struct {
	Server        string
	Statements    []MigrationStatement
	Checksum      string
	Roles         []string
	Grants        []DesiredGrant
	StateChecksum string // fingerprint of the state store at plan time; empty if no prior state
}

// BuildDesiredState computes the desired grants for a server given the config,
// programs, and environment. Templated roles (containing {{name}}) are expanded
// per program and their grants are scoped to that program's databases.
func BuildDesiredState(
	serverName string,
	env string,
	cfg *config.Config,
	progs config.ProgramsFile,
) *DesiredState {
	expandedRoles := config.ExpandRolesForServer(cfg.Roles, progs, serverName, env)
	programDBs := config.BuildProgramDBMap(serverName, env, progs)
	roleProgMap := config.BuildRoleProgramMap(cfg.Roles, programDBs)

	return buildDesiredStateInternal(serverName, expandedRoles, programDBs, roleProgMap, cfg.PermissionSets)
}

// BuildDesiredStateFromExpanded computes the desired grants using pre-expanded roles
// and program DBs, avoiding redundant recomputation. This is the fast path used by
// the plan command which already has expanded roles available.
func BuildDesiredStateFromExpanded(
	serverName string,
	templateRoles []config.RoleConfig,
	expandedRoles []config.RoleConfig,
	programDBs map[string]config.ProgramDBs,
	permissionSets map[string][]string,
) *DesiredState {
	roleProgMap := config.BuildRoleProgramMap(templateRoles, programDBs)

	return buildDesiredStateInternal(serverName, expandedRoles, programDBs, roleProgMap, permissionSets)
}

func buildDesiredStateInternal(
	serverName string,
	expandedRoles []config.RoleConfig,
	programDBs map[string]config.ProgramDBs,
	roleProgMap map[string]string,
	permissionSets map[string][]string,
) *DesiredState {
	state := &DesiredState{
		Server: serverName,
	}

	roleSet := make(map[string]struct{})

	for _, role := range expandedRoles {
		roleSet[role.Name] = struct{}{}

		var pdbs map[string]config.ProgramDBs
		if progName, ok := roleProgMap[role.Name]; ok {
			// Templated role: scope to this program's DBs only
			pdbs = map[string]config.ProgramDBs{
				progName: programDBs[progName],
			}
		} else {
			// Non-templated role: apply across all programs
			pdbs = programDBs
		}

		grants := buildRoleGrants(role, pdbs, permissionSets)
		state.Grants = append(state.Grants, grants...)
	}

	for r := range roleSet {
		state.Roles = append(state.Roles, r)
	}
	sort.Strings(state.Roles)

	// Sort grants for deterministic output: by role, database, table.
	sort.Slice(state.Grants, func(i, j int) bool {
		if state.Grants[i].Role != state.Grants[j].Role {
			return state.Grants[i].Role < state.Grants[j].Role
		}
		if state.Grants[i].Database != state.Grants[j].Database {
			return state.Grants[i].Database < state.Grants[j].Database
		}
		return state.Grants[i].Table < state.Grants[j].Table
	})

	return state
}

func buildRoleGrants(
	role config.RoleConfig,
	programDBs map[string]config.ProgramDBs,
	permissionSets map[string][]string,
) []DesiredGrant {
	var grants []DesiredGrant

	// Server-level grants (*.*)
	if perms, ok := role.Server["*"]; ok {
		resolved := ResolvePermissionSets(perms, permissionSets)
		if len(resolved) > 0 {
			grants = append(grants, DesiredGrant{
				Role:       role.Name,
				Database:   "*",
				Table:      "",
				Privileges: resolved,
			})
		}
	}

	// Server-scope non-wildcard keys: 'scratch.*' → database=scratch, table=*
	for key, perms := range role.Server {
		if key == "*" {
			continue
		}
		resolved := ResolvePermissionSets(perms, permissionSets)
		if len(resolved) == 0 {
			continue
		}
		db, tbl := parseServerScopeKey(key)
		grants = append(grants, DesiredGrant{
			Role:       role.Name,
			Database:   db,
			Table:      tbl,
			Privileges: resolved,
		})
	}

	// Collect all app_db and sup_db schema names from programs
	var allAppDBs, allSupDBs []string
	for _, pdbs := range programDBs {
		allAppDBs = append(allAppDBs, pdbs.AppDBs...)
		allSupDBs = append(allSupDBs, pdbs.SupDBs...)
	}

	// App DB grants: scope keys are table names within app_db schemas
	appGrants := buildScopeGrants(role.Name, role.AppDB, allAppDBs, permissionSets)
	grants = append(grants, appGrants...)

	// Sup DB grants: scope keys are table names within sup_db schemas
	supGrants := buildScopeGrants(role.Name, role.SupDB, allSupDBs, permissionSets)
	grants = append(grants, supGrants...)

	return grants
}

// buildScopeGrants builds grants for a role's scope (app_db or sup_db).
// scopePerms maps table names → permission set names:
//   - '*' means all tables in the schema → GRANT perms ON schema.*
//   - specific table name → GRANT perms ON schema.table
//   - pattern like 'dst_%' → GRANT perms ON schema.table for matching tables
//
// schemas are the schema names from the program definition.
func buildScopeGrants(
	roleName string,
	scopePerms map[string][]string,
	schemas []string,
	permissionSets map[string][]string,
) []DesiredGrant {
	if len(scopePerms) == 0 || len(schemas) == 0 {
		return nil
	}

	var grants []DesiredGrant
	seen := make(map[string]struct{}) // "schema.table" to deduplicate

	for _, schema := range schemas {
		// Wildcard '*' table: grant on all tables in schema (schema.*)
		if perms, ok := scopePerms["*"]; ok {
			resolved := ResolvePermissionSets(perms, permissionSets)
			grants = addScopeGrant(grants, seen, roleName, schema, "*", resolved)
		}

		// Specific table names and patterns
		for tableKey, permNames := range scopePerms {
			if tableKey == "*" {
				continue // already handled above
			}
			resolved := ResolvePermissionSets(permNames, permissionSets)
			grants = addScopeGrant(grants, seen, roleName, schema, tableKey, resolved)
		}
	}

	return grants
}

// addScopeGrant appends a DesiredGrant with deduplication by "schema.table" key.
// Returns the updated grants slice.
func addScopeGrant(
	grants []DesiredGrant,
	seen map[string]struct{},
	roleName, schema, tableKey string,
	resolved []string,
) []DesiredGrant {
	if len(resolved) == 0 {
		return grants
	}
	key := schema + "." + tableKey
	if _, dup := seen[key]; dup {
		return grants
	}
	seen[key] = struct{}{}

	return append(grants, DesiredGrant{
		Role:       roleName,
		Database:   schema,
		Table:      tableKey,
		Privileges: resolved,
	})
}

func LikeMatch(value, pattern string) bool {
	return likeMatchRecursive(value, pattern, 0, 0)
}

// likeMatchPercent handles the % wildcard in LIKE pattern matching.
// It skips consecutive %, then tries to match the rest of the pattern
// at each remaining position in the value string.
func likeMatchPercent(value, pattern string, vi, pi int) bool {
	// Skip consecutive %
	for pi < len(pattern) && pattern[pi] == '%' {
		pi++
	}
	if pi >= len(pattern) {
		return true // % at end matches everything
	}
	// Try matching rest of pattern at each position
	for vi <= len(value) {
		if likeMatchRecursive(value, pattern, vi, pi) {
			return true
		}
		vi++
	}
	return false
}

func likeMatchRecursive(value, pattern string, vi, pi int) bool {
	for pi < len(pattern) && vi < len(value) {
		if pattern[pi] == '%' {
			return likeMatchPercent(value, pattern, vi, pi)
		}
		if pattern[pi] == '_' {
			pi++
			vi++
			continue
		}
		if pattern[pi] != value[vi] {
			return false
		}
		pi++
		vi++
	}

	// Skip trailing %
	for pi < len(pattern) && pattern[pi] == '%' {
		pi++
	}

	return pi == len(pattern) && vi == len(value)
}

// IsDatabasePattern returns true if the database name contains LIKE wildcard
// characters that need server-side expansion. Currently only `%` is treated as
// a wildcard; underscores in database names are too common to interpret as the
// LIKE `_` wildcard, so they are left literal.
func IsDatabasePattern(db string) bool {
	return strings.Contains(db, "%")
}

// ExpandDatabasePatterns expands desired grants whose Database field contains a
// LIKE wildcard (`%`) into concrete grants for each matching database on the
// server. Grants with table="*" are expanded too (MySQL does not support LIKE
// patterns in GRANT identifiers).  Grants whose database is a plain literal
// name are left untouched.
//
// dbNames is the list of databases returned by mysql.QueryDatabases.
func ExpandDatabasePatterns(state *DesiredState, dbNames []string) {
	// Build a sorted list of database names for deterministic output
	sorted := make([]string, len(dbNames))
	copy(sorted, dbNames)
	sort.Strings(sorted)

	var expanded []DesiredGrant
	for _, g := range state.Grants {
		if !IsDatabasePattern(g.Database) {
			expanded = append(expanded, g)
			continue
		}
		// Pattern database: find all matching databases on the server
		for _, dbName := range sorted {
			if likeMatchDB(dbName, g.Database) {
				expanded = append(expanded, DesiredGrant{
					Role:       g.Role,
					Database:   dbName,
					Table:      g.Table,
					Privileges: g.Privileges,
				})
			}
		}
	}
	state.Grants = expanded
}

// likeMatchDB matches a concrete database name against a LIKE pattern.
// Only `%` is treated as a wildcard; `_` matches literally because
// underscores are extremely common in database names.
func likeMatchDB(value, pattern string) bool {
	return likeMatchDBRecursive(value, pattern, 0, 0)
}

func likeMatchDBPercent(value, pattern string, vi, pi int) bool {
	for pi < len(pattern) && pattern[pi] == '%' {
		pi++
	}
	if pi >= len(pattern) {
		return true
	}
	for vi <= len(value) {
		if likeMatchDBRecursive(value, pattern, vi, pi) {
			return true
		}
		vi++
	}
	return false
}

func likeMatchDBRecursive(value, pattern string, vi, pi int) bool {
	for pi < len(pattern) && vi < len(value) {
		if pattern[pi] == '%' {
			return likeMatchDBPercent(value, pattern, vi, pi)
		}
		if pattern[pi] != value[vi] {
			return false
		}
		pi++
		vi++
	}
	for pi < len(pattern) && pattern[pi] == '%' {
		pi++
	}
	return pi == len(pattern) && vi == len(value)
}

func ResolvePermissionSets(names []string, permissionSets map[string][]string) []string {
	var result []string
	seen := make(map[string]struct{})
	for _, name := range names {
		if perms, permsOk := permissionSets[name]; permsOk {
			for _, p := range perms {
				if _, ok := seen[p]; !ok {
					result = append(result, p)
					seen[p] = struct{}{}
				}
			}
		} else {
			// Treat as a raw MySQL privilege name
			if _, ok := seen[name]; !ok {
				result = append(result, name)
				seen[name] = struct{}{}
			}
		}
	}
	return result
}

// Diff computes the migration statements needed to bring the actual state to the desired state.
// This is the primary diff: desired (config) vs actual (live server).
func Diff(desired *DesiredState, actual *mysql.ActualState, dropRoles bool) []MigrationStatement {
	actualEntries := make([]GrantEntry, len(actual.Grants))
	for i, g := range actual.Grants {
		actualEntries[i] = GrantEntry{
			Role:       g.Role,
			Database:   g.Database,
			Table:      g.Table,
			Privileges: g.Grants,
		}
	}
	return DiffGrants(desired.Roles, desiredGrantsToEntries(desired.Grants), actual.Roles, actualEntries, dropRoles)
}

func desiredGrantsToEntries(grants []DesiredGrant) []GrantEntry {
	entries := make([]GrantEntry, len(grants))
	for i, g := range grants {
		entries[i] = GrantEntry(g)
	}
	return entries
}

// GrantMapKey produces a unique key for a grant target within a role's grants.
func GrantMapKey(database, table string) string {
	if database == "*" {
		return "*.*"
	}
	if table == "" {
		return database
	}
	return database + "." + table
}

func diffPrivileges(desired []string, actual map[string]struct{}) ([]string, []string) {
	desiredSet := make(map[string]struct{})
	for _, p := range desired {
		desiredSet[p] = struct{}{}
	}

	toGrant := []string{}
	toRevoke := []string{}

	// USAGE is a no-op sentinel in MySQL: it is implicitly present whenever any
	// other privilege exists on the same target.  Check whether the actual set
	// contains a real (non-USAGE) privilege so we can skip granting USAGE when
	// it is already subsumed.
	hasActualPrivilege := false
	for p := range actual {
		if p != "USAGE" {
			hasActualPrivilege = true
			break
		}
	}

	for _, p := range desired {
		if _, ok := actual[p]; !ok {
			// USAGE is subsumed by any other privilege already present on the target.
			if p == "USAGE" && hasActualPrivilege {
				continue
			}
			toGrant = append(toGrant, p)
		}
	}

	// If ALL PRIVILEGES is desired, any other actual privilege is subsumed
	_, allDesired := desiredSet["ALL PRIVILEGES"]

	for p := range actual {
		if _, ok := desiredSet[p]; !ok {
			if allDesired {
				continue // ALL PRIVILEGES subsumes everything
			}
			// USAGE is always implicit in MySQL; never generate REVOKE USAGE.
			if p == "USAGE" {
				continue
			}
			toRevoke = append(toRevoke, p)
		}
	}

	sort.Strings(toGrant)
	sort.Strings(toRevoke)

	return toGrant, toRevoke
}

func GrantDBRef(database, table string) string {
	if database == "*" {
		return "*.*"
	}
	if table == "*" || table == "" {
		return fmt.Sprintf("`%s`.*", database)
	}
	return fmt.Sprintf("`%s`.`%s`", database, table)
}

// GrantEntry is a generic grant record that can come from desired state or state store.
type GrantEntry struct {
	Role       string
	Database   string
	Table      string
	Privileges []string
}

// DiffGrants computes migration statements to bring the "from" grant set to the "to" grant set.
// This is a general-purpose diff that works on any two grant sets (desired, actual, last-applied).
// "from" is the current/source state, "to" is the target state.
// Roles in "to" not in "from" are created; roles in "from" not in "to" may be dropped.
func DiffGrants(
	toRoles []string,
	toGrants []GrantEntry,
	fromRoles []string,
	fromGrants []GrantEntry,
	dropRoles bool,
) []MigrationStatement {
	var stmts []MigrationStatement

	fromRoleSet := make(map[string]struct{})
	for _, r := range fromRoles {
		fromRoleSet[r] = struct{}{}
	}

	// Create missing roles
	for _, role := range toRoles {
		if _, ok := fromRoleSet[role]; !ok {
			stmts = append(stmts, MigrationStatement{
				SQL:  fmt.Sprintf("CREATE ROLE '%s'", role),
				Type: "create_role",
				Role: role,
			})
		}
	}

	// Build grant maps for both sides
	fromMap := buildGrantEntryMap(fromGrants)
	toMap := buildGrantEntryMap(toGrants)

	// Process all "to" grants
	for role, targets := range toMap {
		for target, privs := range targets {
			fromPrivs := fromMap[role][target]
			toPrivList := sortedKeys(privs)

			toGrant, toRevoke := diffPrivileges(toPrivList, fromPrivs)

			db, tbl := ParseGrantMapKey(target)
			dbRef := GrantDBRef(db, tbl)

			if len(toGrant) > 0 {
				stmts = append(stmts, MigrationStatement{
					SQL:      fmt.Sprintf("GRANT %s ON %s TO '%s'", strings.Join(toGrant, ", "), dbRef, role),
					Type:     "grant",
					Role:     role,
					Database: db,
					Table:    tbl,
				})
			}

			if len(toRevoke) > 0 {
				stmts = append(stmts, MigrationStatement{
					SQL:      fmt.Sprintf("REVOKE %s ON %s FROM '%s'", strings.Join(toRevoke, ", "), dbRef, role),
					Type:     "revoke",
					Role:     role,
					Database: db,
					Table:    tbl,
				})
			}
		}
	}

	// Revoke grants in "from" that are not in "to" at all
	stmts = append(stmts, revokeFromOnlyGrants(fromMap, toMap)...)

	// Drop roles not in "to" config (if enabled)
	stmts = append(stmts, dropMissingRoles(fromRoles, toRoles, dropRoles)...)

	// Sort statements for deterministic output: by type order, then role, database, table.
	sort.Slice(stmts, func(i, j int) bool {
		if stmts[i].Type.CompareOrder() != stmts[j].Type.CompareOrder() {
			return stmts[i].Type.CompareOrder() < stmts[j].Type.CompareOrder()
		}
		if stmts[i].Role != stmts[j].Role {
			return stmts[i].Role < stmts[j].Role
		}
		if stmts[i].Database != stmts[j].Database {
			return stmts[i].Database < stmts[j].Database
		}
		return stmts[i].Table < stmts[j].Table
	})

	return stmts
}

// revokeFromOnlyGrants generates REVOKE statements for grants that exist in "from"
// but have no corresponding entry in "to". This covers two cases:
//  1. A role exists in from but not in to — revoke all its grants.
//  2. A grant target exists in from but not in to for a shared role — revoke that target.
func revokeFromOnlyGrants(
	fromMap, toMap map[string]map[string]map[string]struct{},
) []MigrationStatement {
	var stmts []MigrationStatement

	for role, targets := range fromMap {
		for target, privs := range targets {
			// Skip role+target pairs that still exist in "to" (handled by diffPrivileges)
			if roleTargets, targetOk := toMap[role]; targetOk {
				if _, ok := roleTargets[target]; ok {
					continue
				}
			}

			db, tbl := ParseGrantMapKey(target)
			dbRef := GrantDBRef(db, tbl)
			revList := sortedKeys(privs)
			if len(revList) > 0 {
				stmts = append(stmts, MigrationStatement{
					SQL:      fmt.Sprintf("REVOKE %s ON %s FROM '%s'", strings.Join(revList, ", "), dbRef, role),
					Type:     "revoke",
					Role:     role,
					Database: db,
					Table:    tbl,
				})
			}
		}
	}

	return stmts
}

// dropMissingRoles generates DROP ROLE statements for roles that exist in fromRoles
// but not in toRoles. Only runs when dropRoles is true.
func dropMissingRoles(fromRoles, toRoles []string, dropRoles bool) []MigrationStatement {
	if !dropRoles {
		return nil
	}

	toRoleSet := make(map[string]struct{})
	for _, r := range toRoles {
		toRoleSet[r] = struct{}{}
	}

	var stmts []MigrationStatement
	for _, r := range fromRoles {
		if _, ok := toRoleSet[r]; !ok {
			stmts = append(stmts, MigrationStatement{
				SQL:  fmt.Sprintf("DROP ROLE '%s'", r),
				Type: "drop_role",
				Role: r,
			})
		}
	}

	return stmts
}

func buildGrantEntryMap(grants []GrantEntry) map[string]map[string]map[string]struct{} {
	result := make(map[string]map[string]map[string]struct{})
	for _, g := range grants {
		if result[g.Role] == nil {
			result[g.Role] = make(map[string]map[string]struct{})
		}
		key := GrantMapKey(g.Database, g.Table)
		if result[g.Role][key] == nil {
			result[g.Role][key] = make(map[string]struct{})
		}
		for _, p := range g.Privileges {
			result[g.Role][key][p] = struct{}{}
		}
	}
	return result
}

// ParseGrantMapKey splits a GrantMapKey back into database and table.
func ParseGrantMapKey(key string) (string, string) {
	if key == "*.*" {
		return "*", ""
	}
	before, after, ok := strings.Cut(key, ".")
	if !ok {
		return key, ""
	}
	return before, after
}

func sortedKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// parseServerScopeKey parses a server-scope key like 'scratch.*' or 'mydb.objects'
// into (database, table). 'scratch.*' → ("scratch", "*"), 'mydb.objects' → ("mydb", "objects").
func parseServerScopeKey(key string) (string, string) {
	db, tbl, ok := strings.Cut(key, ".")
	if !ok {
		return key, ""
	}
	return db, tbl
}
