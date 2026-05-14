package migrate_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/na4ma4/mysql-role-reconciler/internal/migrate"
	"github.com/na4ma4/mysql-role-reconciler/internal/reconcile"
	"go.yaml.in/yaml/v3"
)

func TestComputeChecksum(t *testing.T) {
	t.Parallel()
	stmts := []reconcile.MigrationStatement{
		{SQL: "CREATE ROLE 'ro'", Type: "create_role", Role: "ro"},
		{SQL: "GRANT USAGE ON *.* TO 'ro'", Type: "grant", Role: "ro"},
	}

	checksum := migrate.ComputeChecksum(stmts)
	if checksum == "" {
		t.Error("expected non-empty checksum")
	}

	// Same statements in different order should produce same checksum
	reversed := []reconcile.MigrationStatement{
		{SQL: "GRANT USAGE ON *.* TO 'ro'", Type: "grant", Role: "ro"},
		{SQL: "CREATE ROLE 'ro'", Type: "create_role", Role: "ro"},
	}

	checksum2 := migrate.ComputeChecksum(reversed)
	if checksum != checksum2 {
		t.Errorf("checksums should be equal for same statements in different order: %s != %s", checksum, checksum2)
	}
}

func TestWriteAndReadPlanFile(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	planPath := filepath.Join(tmpDir, "plan.json")

	plans := []*reconcile.Plan{
		{
			Server: "hyperion",
			Statements: []reconcile.MigrationStatement{
				{SQL: "CREATE ROLE 'ro'", Type: "create_role", Role: "ro"},
			},
			Checksum: "abc123",
		},
	}

	if err := migrate.WritePlanFile(planPath, "prod", plans); err != nil {
		t.Fatalf("WritePlanFile failed: %v", err)
	}

	plan, err := migrate.ReadPlanFile(planPath)
	if err != nil {
		t.Fatalf("ReadPlanFile failed: %v", err)
	}

	if plan.Environment != "prod" {
		t.Errorf("expected env prod, got %s", plan.Environment)
	}
	if len(plan.Servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(plan.Servers))
	}
	if plan.Servers[0].Server != "hyperion" {
		t.Errorf("expected server hyperion, got %s", plan.Servers[0].Server)
	}
	if len(plan.Servers[0].Statements) != 1 {
		t.Errorf("expected 1 statement, got %d", len(plan.Servers[0].Statements))
	}
}

func TestStateStore(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	ctx := t.Context()
	store := migrate.NewLocalStorage(tmpDir)

	var stateStore *migrate.StateStore
	{
		var err error
		stateStore, err = migrate.LoadStateStore(ctx, store)
		if err != nil {
			t.Fatalf("LoadStateStore failed: %v", err)
		}
	}

	_, exists := stateStore.Get("hyperion")
	if exists {
		t.Error("expected no state for hyperion initially")
	}

	if err := stateStore.Update(ctx, "hyperion", migrate.ServerState{
		Checksum:    "abc123",
		AppliedAt:   "2026-05-14T10:00:00Z",
		Environment: "prod",
		Roles:       []string{"ro"},
		Grants: []migrate.GrantEntry{
			{Role: "ro", Database: "*", Table: "", Privileges: []string{"USAGE"}},
			{Role: "ro", Database: "mydb", Table: "*", Privileges: []string{"SELECT"}},
		},
	}); err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	// Reload and verify
	var stateStore2 *migrate.StateStore
	{
		var err error
		stateStore2, err = migrate.LoadStateStore(ctx, store)
		if err != nil {
			t.Fatalf("LoadStateStore(2) failed: %v", err)
		}
	}

	state, exists := stateStore2.Get("hyperion")
	if !exists {
		t.Fatal("expected state for hyperion after update")
	}
	if state.Checksum != "abc123" {
		t.Errorf("expected checksum abc123, got %s", state.Checksum)
	}
	if state.Environment != "prod" {
		t.Errorf("expected env prod, got %s", state.Environment)
	}
	if len(state.Roles) != 1 || state.Roles[0] != "ro" {
		t.Errorf("expected roles [ro], got %v", state.Roles)
	}
	if len(state.Grants) != 2 {
		t.Errorf("expected 2 grants, got %d", len(state.Grants))
	}
	if state.Grants[0].Database != "*" || state.Grants[0].Privileges[0] != "USAGE" {
		t.Errorf("expected first grant USAGE on *, got %v", state.Grants[0])
	}
}

func TestWriteAndReadHistory(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	ctx := context.Background()
	store := migrate.NewLocalStorage(tmpDir)

	entry := migrate.HistoryEntry{
		Timestamp:   "2026-05-14T10:00:00Z",
		Environment: "prod",
		Server:      "hyperion",
		Statements:  []string{"CREATE ROLE 'ro'", "GRANT USAGE ON *.* TO 'ro'"},
		Checksum:    "abc123",
	}

	if err := migrate.WriteHistory(ctx, store, entry); err != nil {
		t.Fatalf("WriteHistory failed: %v", err)
	}

	var entries []migrate.HistoryEntry
	{
		var err error
		entries, err = migrate.ReadHistory(ctx, store)
		if err != nil {
			t.Fatalf("ReadHistory failed: %v", err)
		}
	}

	if len(entries) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(entries))
	}

	if entries[0].Version != migrate.Version2 {
		t.Errorf("expected version %s, got %s", migrate.Version2, entries[0].Version)
	}
	if entries[0].Server != "hyperion" {
		t.Errorf("expected server hyperion, got %s", entries[0].Server)
	}
	if len(entries[0].Statements) != 2 {
		t.Errorf("expected 2 statements, got %d", len(entries[0].Statements))
	}

	// Verify the written file is valid YAML
	var names []string
	{
		var err error
		names, err = store.ListFiles(ctx, migrate.HistoryPrefix)
		if err != nil {
			t.Fatalf("ListFiles failed: %v", err)
		}
	}
	if len(names) != 1 {
		t.Fatalf("expected 1 history file, got %d", len(names))
	}
	var rawData []byte
	{
		var err error
		rawData, err = store.ReadFile(ctx, migrate.HistoryPrefix+"/"+names[0])
		if err != nil {
			t.Fatalf("reading history file: %v", err)
		}
	}
	var yamlEntry migrate.HistoryEntry
	if err := yaml.Unmarshal(rawData, &yamlEntry); err != nil {
		t.Fatalf("history file should be valid YAML: %v", err)
	}
	if yamlEntry.Server != "hyperion" {
		t.Errorf("YAML entry: expected server hyperion, got %s", yamlEntry.Server)
	}
}

func TestReadHistory_V1Format(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	ctx := context.Background()
	store := migrate.NewLocalStorage(tmpDir)

	// Write a v1-format history file directly (simulating legacy data)
	v1Content := "# Migration applied at 2026-05-14T10:00:00Z\n" +
		"version: \"v1\"\n" +
		"timestamp: \"2026-05-14T10:00:00Z\"\n" +
		"environment: \"prod\"\n" +
		"server: \"hyperion\"\n" +
		"statements:\n" +
		"  - \"CREATE ROLE 'ro'\"\n" +
		"  - \"GRANT USAGE ON *.* TO 'ro'\"\n" +
		"checksum: \"abc123\"\n"

	if err := store.WriteFile(ctx, migrate.HistoryPrefix+"/0001-20260514-100000.yaml", []byte(v1Content)); err != nil {
		t.Fatalf("writing v1 history: %v", err)
	}

	entries, err := migrate.ReadHistory(ctx, store)
	if err != nil {
		t.Fatalf("ReadHistory failed: %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(entries))
	}

	if entries[0].Version != migrate.Version1 {
		t.Errorf("expected version %s, got %s", migrate.Version1, entries[0].Version)
	}
	if entries[0].Server != "hyperion" {
		t.Errorf("expected server hyperion, got %s", entries[0].Server)
	}
	if entries[0].Environment != "prod" {
		t.Errorf("expected environment prod, got %s", entries[0].Environment)
	}
	if len(entries[0].Statements) != 2 {
		t.Errorf("expected 2 statements, got %d", len(entries[0].Statements))
	}
	if entries[0].Checksum != "abc123" {
		t.Errorf("expected checksum abc123, got %s", entries[0].Checksum)
	}
}

func TestReadHistory_MixedV1V2(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	ctx := context.Background()
	store := migrate.NewLocalStorage(tmpDir)

	// Write a v1-format file
	v1Content := "version: \"v1\"\n" +
		"timestamp: \"2026-05-14T10:00:00Z\"\n" +
		"environment: \"prod\"\n" +
		"server: \"srv1\"\n" +
		"statements:\n" +
		"  - \"CREATE ROLE 'ro'\"\n" +
		"checksum: \"aaa\"\n"

	if err := store.WriteFile(ctx, migrate.HistoryPrefix+"/0001-20260514-100000.yaml", []byte(v1Content)); err != nil {
		t.Fatalf("writing v1 history: %v", err)
	}

	// Write a v2-format entry via WriteHistory
	if err := migrate.WriteHistory(ctx, store, migrate.HistoryEntry{
		Timestamp:   "2026-05-15T10:00:00Z",
		Environment: "prod",
		Server:      "srv2",
		Statements:  []string{"CREATE ROLE 'admin'"},
		Checksum:    "bbb",
	}); err != nil {
		t.Fatalf("WriteHistory failed: %v", err)
	}

	entries, err := migrate.ReadHistory(ctx, store)
	if err != nil {
		t.Fatalf("ReadHistory failed: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("expected 2 history entries, got %d", len(entries))
	}

	// v1 entry should come first (sequence-based filename sorts before UUID)
	if entries[0].Server != "srv1" {
		t.Errorf("expected first entry server srv1, got %s", entries[0].Server)
	}
	if entries[0].Version != migrate.Version1 {
		t.Errorf("expected first entry version %s, got %s", migrate.Version1, entries[0].Version)
	}

	// v2 entry should come second
	if entries[1].Server != "srv2" {
		t.Errorf("expected second entry server srv2, got %s", entries[1].Server)
	}
	if entries[1].Version != migrate.Version2 {
		t.Errorf("expected second entry version %s, got %s", migrate.Version2, entries[1].Version)
	}
}

func TestPlanFileJSON(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	planPath := filepath.Join(tmpDir, "plan.json")

	plans := []*reconcile.Plan{
		{
			Server: "hyperion",
			Statements: []reconcile.MigrationStatement{
				{SQL: "CREATE ROLE 'ro'", Type: "create_role", Role: "ro"},
				{SQL: "GRANT SELECT ON `db1`.* TO 'ro'", Type: "grant", Role: "ro", Database: "db1"},
			},
			Checksum: "test-checksum",
		},
	}

	if err := migrate.WritePlanFile(planPath, "prod", plans); err != nil {
		t.Fatalf("WritePlanFile failed: %v", err)
	}

	// Verify it's valid JSON
	var data []byte
	{
		var err error
		data, err = os.ReadFile(planPath)
		if err != nil {
			t.Fatalf("reading plan file: %v", err)
		}
	}

	var pf migrate.PlanFile
	if err := json.Unmarshal(data, &pf); err != nil {
		t.Fatalf("plan file is not valid JSON: %v", err)
	}
}

func TestLocalStorage_ReadWriteFile(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	ctx := context.Background()
	store := migrate.NewLocalStorage(tmpDir)

	content := []byte("hello world")
	if err := store.WriteFile(ctx, "subdir/test.txt", content); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	got, err := store.ReadFile(ctx, "subdir/test.txt")
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	if string(got) != string(content) {
		t.Errorf("expected %q, got %q", content, got)
	}
}

func TestLocalStorage_ListFiles(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	ctx := context.Background()
	store := migrate.NewLocalStorage(tmpDir)

	// Create files in a subdirectory
	if err := store.WriteFile(ctx, "mydir/file1.yaml", []byte("a")); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	if err := store.WriteFile(ctx, "mydir/file2.yaml", []byte("b")); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	// Create a dotfile that should be excluded
	if err := store.WriteFile(ctx, "mydir/.hidden", []byte("c")); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	names, err := store.ListFiles(ctx, "mydir")
	if err != nil {
		t.Fatalf("ListFiles failed: %v", err)
	}

	if len(names) != 2 {
		t.Fatalf("expected 2 files (dotfiles excluded), got %d: %v", len(names), names)
	}
	if names[0] != "file1.yaml" || names[1] != "file2.yaml" {
		t.Errorf("expected [file1.yaml, file2.yaml], got %v", names)
	}
}

func TestLocalStorage_ListFiles_NonexistentDir(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	ctx := context.Background()
	store := migrate.NewLocalStorage(tmpDir)

	names, err := store.ListFiles(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("ListFiles on nonexistent dir should not error: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("expected 0 files, got %d", len(names))
	}
}

func TestNewStorage_Local(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	store, err := migrate.NewStorage(ctx, migrate.StorageConfig{Type: "local", Dir: "."})
	if err != nil {
		t.Fatalf("NewStorage(local) failed: %v", err)
	}
	if _, ok := store.(*migrate.LocalStorage); !ok {
		t.Errorf("expected *LocalStorage, got %T", store)
	}
}

func TestNewStorage_DefaultIsLocal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	store, err := migrate.NewStorage(ctx, migrate.StorageConfig{})
	if err != nil {
		t.Fatalf("NewStorage(default) failed: %v", err)
	}
	if _, ok := store.(*migrate.LocalStorage); !ok {
		t.Errorf("expected *LocalStorage, got %T", store)
	}
}

func TestNewStorage_UnknownType(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	_, err := migrate.NewStorage(ctx, migrate.StorageConfig{Type: "ftp"})
	if err == nil {
		t.Fatal("expected error for unknown storage type")
	}
}

func TestFilterChangedState(t *testing.T) {
	t.Parallel()

	allRoles := []string{"admin", "ro", "unused"}
	allGrants := []migrate.GrantEntry{
		{Role: "admin", Database: "*", Table: "", Privileges: []string{"PROCESS"}},
		{Role: "admin", Database: "db1", Table: "*", Privileges: []string{"SELECT"}},
		{Role: "ro", Database: "db1", Table: "*", Privileges: []string{"SELECT"}},
		{Role: "unused", Database: "db2", Table: "*", Privileges: []string{"SELECT"}},
	}

	tests := []struct {
		name       string
		stmts      []reconcile.MigrationStatement
		wantRoles  []string
		wantGrants int
	}{
		{
			name: "create_role includes all grants for new role",
			stmts: []reconcile.MigrationStatement{
				{Type: "create_role", Role: "admin"},
				{Type: "grant", Role: "admin", Database: "*", Table: ""},
				{Type: "grant", Role: "admin", Database: "db1", Table: "*"},
			},
			wantRoles:  []string{"admin"},
			wantGrants: 2, // both admin grants
		},
		{
			name: "revoke includes only matching grant target",
			stmts: []reconcile.MigrationStatement{
				{Type: "revoke", Role: "admin", Database: "db1", Table: "*"},
			},
			wantRoles:  []string{"admin"},
			wantGrants: 1, // only db1 grant, not * grant
		},
		{
			name: "drop_role includes role but no grants",
			stmts: []reconcile.MigrationStatement{
				{Type: "drop_role", Role: "unused"},
			},
			wantRoles:  []string{"unused"},
			wantGrants: 0,
		},
		{
			name: "multiple roles filters correctly",
			stmts: []reconcile.MigrationStatement{
				{Type: "create_role", Role: "admin"},
				{Type: "grant", Role: "admin", Database: "*", Table: ""},
				{Type: "grant", Role: "admin", Database: "db1", Table: "*"},
				{Type: "revoke", Role: "ro", Database: "db1", Table: "*"},
			},
			wantRoles:  []string{"admin", "ro"},
			wantGrants: 3, // 2 admin grants + 1 ro grant
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotRoles, gotGrants := migrate.FilterChangedState(allRoles, allGrants, tt.stmts)

			if len(gotRoles) != len(tt.wantRoles) {
				t.Errorf("roles: got %v, want %v", gotRoles, tt.wantRoles)
			}
			if len(gotGrants) != tt.wantGrants {
				t.Errorf("grants: got %d, want %d", len(gotGrants), tt.wantGrants)
			}
		})
	}
}
