package reconcile_test

import (
	"strings"
	"testing"

	"github.com/na4ma4/mysql-role-reconciler/internal/config"
	"github.com/na4ma4/mysql-role-reconciler/internal/mysql"
	"github.com/na4ma4/mysql-role-reconciler/internal/reconcile"
)

func TestResolvePermissionSets(t *testing.T) {
	t.Parallel()
	permissionSets := map[string][]string{
		"select": {"SELECT"},
		"dml":    {"INSERT", "UPDATE", "DELETE"},
		"all":    {"ALL PRIVILEGES"},
	}

	result := reconcile.ResolvePermissionSets([]string{"select", "dml"}, permissionSets)
	if len(result) != 4 {
		t.Errorf("expected 4 privileges, got %d: %v", len(result), result)
	}

	result = reconcile.ResolvePermissionSets([]string{"PROCESS"}, permissionSets)
	if len(result) != 1 || result[0] != "PROCESS" {
		t.Errorf("expected [PROCESS], got %v", result)
	}
}

func TestLikeMatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		value   string
		pattern string
		want    bool
	}{
		{"objects", "objects", true},
		{"objects", "obj%", true},
		{"src_table1", "src_%", true},
		{"imp_schema", "imp_%", true},
		{"other", "src_%", false},
		{"abc", "a%c", true},
		{"ac", "a%c", true},
		{"ab", "a_b", false},
		{"acb", "a_b", true},
		{"", "%", true},
		{"anything", "%", true},
	}

	for _, tt := range tests {
		got := reconcile.LikeMatch(tt.value, tt.pattern)
		if got != tt.want {
			t.Errorf("LikeMatch(%q, %q) = %v, want %v", tt.value, tt.pattern, got, tt.want)
		}
	}
}

func TestGrantMapKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		database string
		table    string
		want     string
	}{
		{"*", "", "*.*"},
		{"mydb", "*", "mydb.*"},
		{"mydb", "mytable", "mydb.mytable"},
	}

	for _, tt := range tests {
		got := reconcile.GrantMapKey(tt.database, tt.table)
		if got != tt.want {
			t.Errorf("GrantMapKey(%q, %q) = %q, want %q", tt.database, tt.table, got, tt.want)
		}
	}
}

func TestGrantDBRef(t *testing.T) {
	t.Parallel()
	tests := []struct {
		database string
		table    string
		want     string
	}{
		{"*", "", "*.*"},
		{"mydb", "*", "`mydb`.*"},
		{"mydb", "mytable", "`mydb`.`mytable`"},
	}

	for _, tt := range tests {
		got := reconcile.GrantDBRef(tt.database, tt.table)
		if got != tt.want {
			t.Errorf("GrantDBRef(%q, %q) = %q, want %q", tt.database, tt.table, got, tt.want)
		}
	}
}

func TestBuildDesiredState(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		PermissionSets: map[string][]string{
			"usage":       {"USAGE"},
			"select":      {"SELECT"},
			"create_temp": {"CREATE TEMPORARY TABLES"},
			"dml":         {"INSERT", "UPDATE", "DELETE"},
		},
		Roles: []config.RoleConfig{
			{
				Name:   "ro",
				Server: map[string][]string{"*": {"usage"}},
				AppDB:  map[string][]string{"*": {"select", "create_temp"}},
				SupDB:  map[string][]string{"*": {"select", "create_temp"}},
			},
		},
	}

	progs := config.ProgramsFile{
		{
			Name:   "app1",
			Server: map[string]string{"prod": "rdsserver1"},
			AppDB:  []string{"db_app1"},
			SupDB:  []string{"sup_app1"},
		},
	}

	state := reconcile.BuildDesiredState("rdsserver1", "prod", cfg, progs)

	if state.Server != "rdsserver1" {
		t.Errorf("expected server rdsserver1, got %s", state.Server)
	}
	if len(state.Roles) != 1 || state.Roles[0] != "ro" {
		t.Errorf("expected role [ro], got %v", state.Roles)
	}

	// Should have: server-level grant + app_db (schema.* wildcard) + sup_db (schema.* wildcard) = 3 grants
	if len(state.Grants) != 3 {
		t.Errorf("expected 3 grants, got %d: %v", len(state.Grants), state.Grants)
	}

	// Check the app_db grant is schema-level (table="*")
	foundAppDBGrant := false
	for _, g := range state.Grants {
		if g.Database == "db_app1" && g.Table == "*" {
			foundAppDBGrant = true
		}
	}
	if !foundAppDBGrant {
		t.Error("expected app_db grant on db_app1 with table=*")
	}
}

func TestBuildDesiredState_DisabledProgramExcluded(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		PermissionSets: map[string][]string{
			"usage":  {"USAGE"},
			"select": {"SELECT"},
		},
		Roles: []config.RoleConfig{
			{
				Name:   "{{name}}-ro",
				Server: map[string][]string{"*": {"usage"}},
				AppDB:  map[string][]string{"*": {"select"}},
			},
		},
	}

	// Only the active program; disabled one already filtered out by the caller.
	progs := config.ProgramsFile{
		{
			Name:   "active",
			Server: map[string]string{"prod": "rdsserver1"},
			AppDB:  []string{"appname_active"},
		},
	}

	state := reconcile.BuildDesiredState("rdsserver1", "prod", cfg, progs)

	for _, g := range state.Grants {
		if g.Database == "appname_trslot" {
			t.Errorf("disabled program's database appname_trslot should not appear in grants, but found: %+v", g)
		}
	}
	for _, r := range state.Roles {
		if r == "trslot-ro" {
			t.Error("disabled program's role trslot-ro should not appear in roles")
		}
	}
}

func TestBuildDesiredState_TemplatedRoles(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		PermissionSets: map[string][]string{
			"usage":       {"USAGE"},
			"select":      {"SELECT"},
			"create_temp": {"CREATE TEMPORARY TABLES"},
			"dml":         {"INSERT", "UPDATE", "DELETE"},
		},
		Roles: []config.RoleConfig{
			{
				Name:   "{{name}}-adm",
				Server: map[string][]string{"*": {"usage"}},
				AppDB:  map[string][]string{"*": {"select", "dml"}},
				SupDB:  map[string][]string{"*": {"select"}},
			},
			{
				Name:   "{{name}}-ro",
				Server: map[string][]string{"*": {"usage"}},
				AppDB:  map[string][]string{"*": {"select"}},
				SupDB:  map[string][]string{"*": {"select"}},
			},
		},
	}

	progs := config.ProgramsFile{
		{
			Name:   "app1",
			Server: map[string]string{"prod": "rdsserver1"},
			AppDB:  []string{"db_app1"},
			SupDB:  []string{"sup_app1"},
		},
		{
			Name:   "app2",
			Server: map[string]string{"prod": "rdsserver1"},
			AppDB:  []string{"db_app2"},
			SupDB:  []string{"sup_app2"},
		},
	}

	state := reconcile.BuildDesiredState("rdsserver1", "prod", cfg, progs)

	if len(state.Roles) != 4 {
		t.Fatalf("expected 4 roles, got %d: %v", len(state.Roles), state.Roles)
	}

	// app1-adm should have: server-level + db_app1.* (app_db) + sup_app1.* (sup_db) = 3 grants
	admApp1Grants := 0
	for _, g := range state.Grants {
		if g.Role == "app1-adm" {
			admApp1Grants++
		}
	}
	if admApp1Grants != 3 {
		t.Errorf("expected 3 grants for app1-adm, got %d", admApp1Grants)
	}

	// app2-adm should NOT have db_app1 in its grants
	for _, g := range state.Grants {
		if g.Role == "app2-adm" && g.Database == "db_app1" {
			t.Errorf("app2-adm should not have grants on db_app1")
		}
	}
}

func TestBuildDesiredState_TableLevelGrants(t *testing.T) {
	t.Parallel()
	// {{name}}-mnt scenario: role has specific TABLE overrides in app_db
	cfg := &config.Config{
		PermissionSets: map[string][]string{
			"usage":       {"USAGE"},
			"select":      {"SELECT"},
			"create_temp": {"CREATE TEMPORARY TABLES"},
			"dml":         {"INSERT", "UPDATE", "DELETE"},
			"ddl":         {"CREATE", "ALTER", "DROP"},
		},
		Roles: []config.RoleConfig{
			{
				Name:   "{{name}}-mnt",
				Server: map[string][]string{"*": {"usage"}},
				AppDB: map[string][]string{
					"*":          {"select", "create_temp"},
					"objects":    {"select", "dml"},
					"operations": {"select", "dml"},
					"users":      {"select", "dml"},
				},
				SupDB: map[string][]string{
					"*":     {"select", "create_temp"},
					"src_%": {"select", "dml", "ddl"},
				},
			},
		},
	}

	progs := config.ProgramsFile{
		{
			Name:   "myapp",
			Server: map[string]string{"prod": "rdsserver1"},
			AppDB:  []string{"appname_myapp"},
			SupDB:  []string{"myapp"},
		},
	}

	state := reconcile.BuildDesiredState("rdsserver1", "prod", cfg, progs)

	roleSet := make(map[string]struct{})
	for _, r := range state.Roles {
		roleSet[r] = struct{}{}
	}
	if _, ok := roleSet["myapp-mnt"]; !ok {
		t.Fatalf("expected role myapp-mnt, got %v", state.Roles)
	}

	// Collect grants for myapp-mnt as schema.table → privileges
	grantMap := make(map[string][]string)
	for _, g := range state.Grants {
		if g.Role == "myapp-mnt" {
			key := reconcile.GrantMapKey(g.Database, g.Table)
			grantMap[key] = g.Privileges
		}
	}

	// Server-level grant
	if _, ok := grantMap["*.*"]; !ok {
		t.Error("expected server-level grant (*.*)")
	}

	// app_db schema wildcard: appname_myapp.* → select, create_temp
	if perms, ok := grantMap["appname_myapp.*"]; !ok {
		t.Error("expected grant on appname_myapp.* (app_db wildcard tables)")
	} else if len(perms) != 2 {
		t.Errorf("expected 2 perms on appname_myapp.*, got %d: %v", len(perms), perms)
	}

	// Table-level grants within the app_db schema
	for _, table := range []string{"objects", "operations", "users"} {
		key := "appname_myapp." + table
		if perms, ok := grantMap[key]; !ok {
			t.Errorf("expected table-level grant on %s", key)
		} else if len(perms) != 4 { // SELECT + INSERT + UPDATE + DELETE
			t.Errorf("expected 4 perms on %s, got %d: %v", key, len(perms), perms)
		}
	}

	// sup_db schema wildcard: myapp.* → select, create_temp
	if perms, ok := grantMap["myapp.*"]; !ok {
		t.Error("expected grant on myapp.* (sup_db wildcard tables)")
	} else if len(perms) != 2 {
		t.Errorf("expected 2 perms on myapp.*, got %d: %v", len(perms), perms)
	}

	// sup_db table pattern: myapp.src_% → select, dml, ddl
	if perms, ok := grantMap["myapp.src_%"]; !ok {
		t.Error("expected grant on myapp.src_% (sup_db pattern tables)")
	} else if len(perms) != 7 { // SELECT + INSERT + UPDATE + DELETE + CREATE + ALTER + DROP
		t.Errorf("expected 7 perms on myapp.src_%%, got %d: %v", len(perms), perms)
	}
}

func TestDiff_CreateRoleAndGrant(t *testing.T) {
	t.Parallel()
	desired := &reconcile.DesiredState{
		Server: "rdsserver1",
		Roles:  []string{"ro"},
		Grants: []reconcile.DesiredGrant{
			{Role: "ro", Database: "*", Table: "", Privileges: []string{"USAGE"}},
		},
	}

	actual := &mysql.ActualState{
		Roles:  []string{},
		Grants: nil,
	}

	stmts := reconcile.Diff(desired, actual, false)

	if len(stmts) < 2 {
		t.Fatalf("expected at least 2 statements, got %d", len(stmts))
	}

	if stmts[0].Type != "create_role" {
		t.Errorf("expected first statement to be create_role, got %s", stmts[0].Type)
	}

	foundGrant := false
	for _, s := range stmts {
		if s.Type == "grant" {
			foundGrant = true
			if s.SQL != "GRANT USAGE ON *.* TO 'ro'" {
				t.Errorf("unexpected grant SQL: %s", s.SQL)
			}
		}
	}
	if !foundGrant {
		t.Error("expected a grant statement")
	}
}

func TestDiff_NoChanges(t *testing.T) {
	t.Parallel()
	desired := &reconcile.DesiredState{
		Server: "rdsserver1",
		Roles:  []string{"ro"},
		Grants: []reconcile.DesiredGrant{
			{Role: "ro", Database: "*", Table: "", Privileges: []string{"USAGE"}},
		},
	}

	actual := &mysql.ActualState{
		Roles: []string{"ro"},
		Grants: []mysql.Grant{
			{Role: "ro", Database: "*", Table: "", Grants: []string{"USAGE"}},
		},
	}

	stmts := reconcile.Diff(desired, actual, false)

	if len(stmts) != 0 {
		t.Errorf("expected 0 statements, got %d: %v", len(stmts), stmts)
	}
}

func TestDiff_TableLevelGrant(t *testing.T) {
	t.Parallel()
	desired := &reconcile.DesiredState{
		Server: "rdsserver1",
		Roles:  []string{"ro"},
		Grants: []reconcile.DesiredGrant{
			{Role: "ro", Database: "mydb", Table: "objects", Privileges: []string{"SELECT", "INSERT"}},
		},
	}

	actual := &mysql.ActualState{
		Roles:  []string{"ro"},
		Grants: []mysql.Grant{},
	}

	stmts := reconcile.Diff(desired, actual, false)

	foundGrant := false
	for _, s := range stmts {
		if s.Type == "grant" && s.Table == "objects" {
			foundGrant = true
			expected := "GRANT INSERT, SELECT ON `mydb`.`objects` TO 'ro'"
			if s.SQL != expected {
				t.Errorf("expected %q, got %q", expected, s.SQL)
			}
		}
	}
	if !foundGrant {
		t.Error("expected table-level grant statement")
	}
}

func TestDiff_Revoke(t *testing.T) {
	t.Parallel()
	desired := &reconcile.DesiredState{
		Server: "rdsserver1",
		Roles:  []string{"ro"},
		Grants: []reconcile.DesiredGrant{
			{Role: "ro", Database: "*", Table: "", Privileges: []string{"USAGE"}},
		},
	}

	actual := &mysql.ActualState{
		Roles: []string{"ro"},
		Grants: []mysql.Grant{
			{Role: "ro", Database: "*", Table: "", Grants: []string{"USAGE", "PROCESS"}},
		},
	}

	stmts := reconcile.Diff(desired, actual, false)

	hasRevoke := false
	for _, s := range stmts {
		if s.Type == "revoke" && s.Role == "ro" {
			hasRevoke = true
		}
	}
	if !hasRevoke {
		t.Errorf("expected revoke statement, got %v", stmts)
	}
}

func TestDiff_DropRole(t *testing.T) {
	t.Parallel()
	desired := &reconcile.DesiredState{
		Server: "rdsserver1",
		Roles:  []string{},
		Grants: nil,
	}

	actual := &mysql.ActualState{
		Roles:  []string{"old_role"},
		Grants: []mysql.Grant{},
	}

	stmts := reconcile.Diff(desired, actual, true)

	hasDrop := false
	for _, s := range stmts {
		if s.Type == "drop_role" && s.Role == "old_role" {
			hasDrop = true
		}
	}
	if !hasDrop {
		t.Errorf("expected drop_role statement")
	}
}

func TestDiff_DropRole_Disabled(t *testing.T) {
	t.Parallel()
	desired := &reconcile.DesiredState{
		Server: "rdsserver1",
		Roles:  []string{},
		Grants: nil,
	}

	actual := &mysql.ActualState{
		Roles:  []string{"old_role"},
		Grants: []mysql.Grant{},
	}

	stmts := reconcile.Diff(desired, actual, false)

	for _, s := range stmts {
		if s.Type == "drop_role" {
			t.Errorf("did not expect drop_role when disabled, got %v", s)
		}
	}
}

func TestDiffGrants_ConfigDrift(t *testing.T) {
	t.Parallel()
	// Last-applied state: ro has USAGE and SELECT on mydb.*
	lastRoles := []string{"ro"}
	lastGrants := []reconcile.GrantEntry{
		{Role: "ro", Database: "*", Table: "", Privileges: []string{"USAGE"}},
		{Role: "ro", Database: "mydb", Table: "*", Privileges: []string{"SELECT"}},
	}

	// Current desired state: ro now also has INSERT on mydb.*
	currentRoles := []string{"ro"}
	currentGrants := []reconcile.GrantEntry{
		{Role: "ro", Database: "*", Table: "", Privileges: []string{"USAGE"}},
		{Role: "ro", Database: "mydb", Table: "*", Privileges: []string{"SELECT", "INSERT"}},
	}

	stmts := reconcile.DiffGrants(currentRoles, currentGrants, lastRoles, lastGrants, false)

	if len(stmts) != 1 {
		t.Fatalf("expected 1 statement (grant INSERT), got %d: %v", len(stmts), stmts)
	}
	if stmts[0].Type != "grant" {
		t.Errorf("expected grant, got %s", stmts[0].Type)
	}
	if stmts[0].SQL != "GRANT INSERT ON `mydb`.* TO 'ro'" {
		t.Errorf("unexpected SQL: %s", stmts[0].SQL)
	}
}

func TestDiffGrants_ServerDrift(t *testing.T) {
	t.Parallel()
	// Last-applied desired: ro has USAGE and SELECT
	lastRoles := []string{"ro"}
	lastGrants := []reconcile.GrantEntry{
		{Role: "ro", Database: "*", Table: "", Privileges: []string{"USAGE"}},
		{Role: "ro", Database: "mydb", Table: "*", Privileges: []string{"SELECT"}},
	}

	// Current server state: someone added PROCESS manually
	currentRoles := []string{"ro"}
	currentGrants := []reconcile.GrantEntry{
		{Role: "ro", Database: "*", Table: "", Privileges: []string{"USAGE", "PROCESS"}},
		{Role: "ro", Database: "mydb", Table: "*", Privileges: []string{"SELECT"}},
	}

	// Diff last-applied desired vs current actual shows server drift
	stmts := reconcile.DiffGrants(lastRoles, lastGrants, currentRoles, currentGrants, false)

	foundRevoke := false
	for _, s := range stmts {
		if s.Type == "revoke" && strings.Contains(s.SQL, "PROCESS") {
			foundRevoke = true
		}
	}
	if !foundRevoke {
		t.Errorf("expected revoke for PROCESS (server drift), got %v", stmts)
	}
}

func TestDiffGrants_NewRole(t *testing.T) {
	t.Parallel()
	lastRoles := []string{"ro"}
	lastGrants := []reconcile.GrantEntry{
		{Role: "ro", Database: "*", Table: "", Privileges: []string{"USAGE"}},
	}

	currentRoles := []string{"ro", "mnt"}
	currentGrants := []reconcile.GrantEntry{
		{Role: "ro", Database: "*", Table: "", Privileges: []string{"USAGE"}},
		{Role: "mnt", Database: "mydb", Table: "*", Privileges: []string{"SELECT"}},
	}

	stmts := reconcile.DiffGrants(currentRoles, currentGrants, lastRoles, lastGrants, false)

	foundCreate := false
	foundGrant := false
	for _, s := range stmts {
		if s.Type == "create_role" && s.Role == "mnt" {
			foundCreate = true
		}
		if s.Type == "grant" && s.Role == "mnt" {
			foundGrant = true
		}
	}
	if !foundCreate {
		t.Error("expected CREATE ROLE for new role mnt")
	}
	if !foundGrant {
		t.Error("expected GRANT for new role mnt")
	}
}

func TestDiffGrants_NoDrift(t *testing.T) {
	t.Parallel()
	roles := []string{"ro"}
	grants := []reconcile.GrantEntry{
		{Role: "ro", Database: "*", Table: "", Privileges: []string{"USAGE"}},
	}

	stmts := reconcile.DiffGrants(roles, grants, roles, grants, false)
	if len(stmts) != 0 {
		t.Errorf("expected 0 statements (no drift), got %d: %v", len(stmts), stmts)
	}
}

func TestParseGrantMapKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		key       string
		wantDB    string
		wantTable string
	}{
		{"*.*", "*", ""},
		{"mydb.*", "mydb", "*"},
		{"mydb.objects", "mydb", "objects"},
	}

	for _, tt := range tests {
		gotDB, gotTable := reconcile.ParseGrantMapKey(tt.key)
		if gotDB != tt.wantDB || gotTable != tt.wantTable {
			t.Errorf("ParseGrantMapKey(%q) = (%q, %q), want (%q, %q)",
				tt.key, gotDB, gotTable, tt.wantDB, tt.wantTable)
		}
	}
}

func TestDiff_UsageSubsumedByOtherPrivilege(t *testing.T) {
	t.Parallel()
	// BUG FIX: When PROCESS is already granted on *.*, USAGE should NOT be
	// detected as needing to be added.  USAGE is a no-op sentinel that MySQL
	// implicitly includes whenever any other privilege exists on the target.
	desired := &reconcile.DesiredState{
		Server: "rdsserver1",
		Roles:  []string{"myapp-adm"},
		Grants: []reconcile.DesiredGrant{
			{Role: "myapp-adm", Database: "*", Table: "", Privileges: []string{"USAGE", "PROCESS"}},
		},
	}

	actual := &mysql.ActualState{
		Roles: []string{"myapp-adm"},
		Grants: []mysql.Grant{
			{Role: "myapp-adm", Database: "*", Table: "", Grants: []string{"PROCESS"}},
		},
	}

	stmts := reconcile.Diff(desired, actual, false)

	for _, s := range stmts {
		if strings.Contains(s.SQL, "USAGE") {
			t.Errorf("USAGE should not appear in any statement when PROCESS is already present, got: %s", s.SQL)
		}
	}
}

func TestDiff_UsageSubsumed_ProcessPresent(t *testing.T) {
	t.Parallel()
	// Desired wants only USAGE on *.*, but actual already has PROCESS.
	// USAGE is subsumed by PROCESS — no GRANT USAGE should be generated.
	// However, PROCESS should be revoked since it's not in desired.
	desired := &reconcile.DesiredState{
		Server: "rdsserver1",
		Roles:  []string{"ro"},
		Grants: []reconcile.DesiredGrant{
			{Role: "ro", Database: "*", Table: "", Privileges: []string{"USAGE"}},
		},
	}

	actual := &mysql.ActualState{
		Roles: []string{"ro"},
		Grants: []mysql.Grant{
			{Role: "ro", Database: "*", Table: "", Grants: []string{"PROCESS"}},
		},
	}

	stmts := reconcile.Diff(desired, actual, false)

	for _, s := range stmts {
		if strings.Contains(s.SQL, "USAGE") {
			t.Errorf("USAGE should not appear in any statement, got: %s", s.SQL)
		}
	}
	if len(stmts) != 1 {
		t.Fatalf("expected 1 statement (REVOKE PROCESS), got %d: %v", len(stmts), stmts)
	}
	if stmts[0].SQL != "REVOKE PROCESS ON *.* FROM 'ro'" {
		t.Errorf("expected REVOKE PROCESS, got: %s", stmts[0].SQL)
	}
}

func TestDiff_UsageNotRevoked(t *testing.T) {
	t.Parallel()
	// Actual has USAGE + PROCESS on *.*, desired has only PROCESS.
	// USAGE should NOT be revoked — it's always implicit in MySQL.
	desired := &reconcile.DesiredState{
		Server: "rdsserver1",
		Roles:  []string{"ro"},
		Grants: []reconcile.DesiredGrant{
			{Role: "ro", Database: "*", Table: "", Privileges: []string{"PROCESS"}},
		},
	}

	actual := &mysql.ActualState{
		Roles: []string{"ro"},
		Grants: []mysql.Grant{
			{Role: "ro", Database: "*", Table: "", Grants: []string{"USAGE", "PROCESS"}},
		},
	}

	stmts := reconcile.Diff(desired, actual, false)

	for _, s := range stmts {
		if strings.Contains(s.SQL, "USAGE") {
			t.Errorf("USAGE should not appear in any revoke statement, got: %s", s.SQL)
		}
	}
	if len(stmts) != 0 {
		t.Errorf("expected 0 statements (no drift), got %d: %v", len(stmts), stmts)
	}
}

func TestDiff_UsageGrantedWhenNoOtherPrivilege(t *testing.T) {
	t.Parallel()
	// When a role is new and USAGE is the only desired privilege on *.*,
	// GRANT USAGE should still be generated (nothing subsumes it).
	desired := &reconcile.DesiredState{
		Server: "rdsserver1",
		Roles:  []string{"ro"},
		Grants: []reconcile.DesiredGrant{
			{Role: "ro", Database: "*", Table: "", Privileges: []string{"USAGE"}},
		},
	}

	actual := &mysql.ActualState{
		Roles:  []string{"ro"},
		Grants: []mysql.Grant{},
	}

	stmts := reconcile.Diff(desired, actual, false)

	foundGrantUsage := false
	for _, s := range stmts {
		if s.Type == "grant" && s.SQL == "GRANT USAGE ON *.* TO 'ro'" {
			foundGrantUsage = true
		}
	}
	if !foundGrantUsage {
		t.Errorf("expected GRANT USAGE ON *.* TO 'ro', got %v", stmts)
	}
}

func TestExpandDatabasePatterns_NoPatterns(t *testing.T) {
	t.Parallel()
	state := &reconcile.DesiredState{
		Server: "test",
		Grants: []reconcile.DesiredGrant{
			{Role: "ro", Database: "mydb", Table: "*", Privileges: []string{"SELECT"}},
		},
	}
	dbNames := []string{"mydb", "otherdb"}
	reconcile.ExpandDatabasePatterns(state, dbNames)

	if len(state.Grants) != 1 {
		t.Fatalf("expected 1 grant, got %d", len(state.Grants))
	}
	if state.Grants[0].Database != "mydb" {
		t.Errorf("expected database mydb, got %q", state.Grants[0].Database)
	}
}

func TestExpandDatabasePatterns_PatternWithSpecificTable(t *testing.T) {
	t.Parallel()
	state := &reconcile.DesiredState{
		Server: "test",
		Grants: []reconcile.DesiredGrant{
			{Role: "ro", Database: "appname_qa%", Table: "records", Privileges: []string{"SELECT"}},
		},
	}
	dbNames := []string{"appname_qa1", "appname_qa2", "appname_prod1", "otherdb"}
	reconcile.ExpandDatabasePatterns(state, dbNames)

	if len(state.Grants) != 2 {
		t.Fatalf("expected 2 grants (one per matching DB), got %d: %+v", len(state.Grants), state.Grants)
	}
	if state.Grants[0].Database != "appname_qa1" || state.Grants[0].Table != "records" {
		t.Errorf("expected grant on appname_qa1.records, got %s.%s", state.Grants[0].Database, state.Grants[0].Table)
	}
	if state.Grants[1].Database != "appname_qa2" || state.Grants[1].Table != "records" {
		t.Errorf("expected grant on appname_qa2.records, got %s.%s", state.Grants[1].Database, state.Grants[1].Table)
	}
}

func TestExpandDatabasePatterns_PatternWithWildcardTable(t *testing.T) {
	t.Parallel()
	state := &reconcile.DesiredState{
		Server: "test",
		Grants: []reconcile.DesiredGrant{
			{Role: "ro", Database: "appname_qa%", Table: "*", Privileges: []string{"SELECT"}},
		},
	}
	dbNames := []string{"appname_qa1", "appname_qa2"}
	reconcile.ExpandDatabasePatterns(state, dbNames)

	if len(state.Grants) != 2 {
		t.Fatalf("expected 2 grants, got %d", len(state.Grants))
	}
	if state.Grants[0].Database != "appname_qa1" || state.Grants[0].Table != "*" {
		t.Errorf("expected appname_qa1.*, got %s.%s", state.Grants[0].Database, state.Grants[0].Table)
	}
	if state.Grants[1].Database != "appname_qa2" || state.Grants[1].Table != "*" {
		t.Errorf("expected appname_qa2.*, got %s.%s", state.Grants[1].Database, state.Grants[1].Table)
	}
}

func TestExpandDatabasePatterns_UnderscoreIsLiteral(t *testing.T) {
	t.Parallel()
	state := &reconcile.DesiredState{
		Server: "test",
		Grants: []reconcile.DesiredGrant{
			{Role: "ro", Database: "appname_qa_%", Table: "records", Privileges: []string{"SELECT"}},
		},
	}
	dbNames := []string{"appname_qa1", "appname_qa_test", "appname_qa_prod"}
	reconcile.ExpandDatabasePatterns(state, dbNames)

	if len(state.Grants) != 2 {
		t.Fatalf(
			"expected 2 grants (only appname_qa_test and appname_qa_prod), got %d: %+v",
			len(state.Grants),
			state.Grants,
		)
	}
	for _, g := range state.Grants {
		if !strings.HasPrefix(g.Database, "appname_qa_") {
			t.Errorf("expected database to start with appname_qa_, got %q", g.Database)
		}
	}
}

func TestExpandDatabasePatterns_NoMatches(t *testing.T) {
	t.Parallel()
	state := &reconcile.DesiredState{
		Server: "test",
		Grants: []reconcile.DesiredGrant{
			{Role: "ro", Database: "appname_nonexistent%", Table: "records", Privileges: []string{"SELECT"}},
		},
	}
	dbNames := []string{"appname_qa1", "otherdb"}
	reconcile.ExpandDatabasePatterns(state, dbNames)

	if len(state.Grants) != 0 {
		t.Fatalf("expected 0 grants (no matching DBs), got %d", len(state.Grants))
	}
}

func TestExpandDatabasePatterns_Mixed(t *testing.T) {
	t.Parallel()
	state := &reconcile.DesiredState{
		Server: "test",
		Grants: []reconcile.DesiredGrant{
			{Role: "ro", Database: "mydb", Table: "*", Privileges: []string{"SELECT"}},
			{Role: "ro", Database: "appname_qa%", Table: "records", Privileges: []string{"SELECT"}},
			{Role: "ro", Database: "*", Table: "", Privileges: []string{"USAGE"}},
		},
	}
	dbNames := []string{"mydb", "appname_qa1", "appname_qa2", "otherdb"}
	reconcile.ExpandDatabasePatterns(state, dbNames)

	if len(state.Grants) != 4 {
		t.Fatalf(
			"expected 4 grants (1 literal + 2 expanded + 1 server-level), got %d: %+v",
			len(state.Grants),
			state.Grants,
		)
	}
	foundServer := false
	for _, g := range state.Grants {
		if g.Database == "*" && g.Table == "" {
			foundServer = true
		}
	}
	if !foundServer {
		t.Error("expected server-level grant (*, '') to be preserved")
	}
}

func TestIsDatabasePattern(t *testing.T) {
	t.Parallel()

	tests := []struct {
		db    string
		match bool
	}{
		{"mydb", false},
		{"appname_qa%", true},
		{"%", true},
		{"%%", true},
		{"my_db", false},
		{"*", false},
	}
	for _, tt := range tests {
		t.Run(tt.db, func(t *testing.T) {
			t.Parallel()
			got := reconcile.IsDatabasePattern(tt.db)
			if got != tt.match {
				t.Errorf("IsDatabasePattern(%q) = %v, want %v", tt.db, got, tt.match)
			}
		})
	}
}
