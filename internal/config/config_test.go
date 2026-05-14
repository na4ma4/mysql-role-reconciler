package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/na4ma4/mysql-role-reconciler/internal/config"
	"go.yaml.in/yaml/v3"
)

func TestMergePrograms_AppendSupDB(t *testing.T) {
	t.Parallel()
	fileProgs := config.ProgramsFile{
		{Name: "app1", AppDB: []string{"db1"}, SupDB: []string{"sup1"}},
		{Name: "app2", AppDB: []string{"db2"}, SupDB: []string{"sup2"}},
	}

	cfgProgs := []config.ProgramConfig{
		{Name: "app1", SupDB: []string{"sup_extra"}},
		{Name: "app3", AppDB: []string{"db3"}},
	}

	fileProgs = config.MergePrograms(fileProgs, cfgProgs)

	if len(fileProgs) != 3 {
		t.Fatalf("expected 3 programs, got %d", len(fileProgs))
	}

	app1 := fileProgs[0]
	if app1.Name != "app1" {
		t.Errorf("expected app1, got %s", app1.Name)
	}
	found := false
	for _, db := range app1.SupDB {
		if db == "sup_extra" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected sup_extra in app1.SupDB, got %v", app1.SupDB)
	}
	// Original sup1 should still be there
	if app1.SupDB[0] != "sup1" {
		t.Errorf("expected sup1 as first element, got %s", app1.SupDB[0])
	}
}

func TestMergePrograms_DeduplicateSupDB(t *testing.T) {
	t.Parallel()
	fileProgs := config.ProgramsFile{
		{Name: "app1", SupDB: []string{"sup1", "sup2"}},
	}

	cfgProgs := []config.ProgramConfig{
		{Name: "app1", SupDB: []string{"sup1", "sup3"}},
	}

	fileProgs = config.MergePrograms(fileProgs, cfgProgs)

	if len(fileProgs) != 1 {
		t.Fatalf("expected 1 program, got %d", len(fileProgs))
	}

	app1 := fileProgs[0]
	if len(app1.SupDB) != 3 {
		t.Errorf("expected 3 sup_db entries, got %d: %v", len(app1.SupDB), app1.SupDB)
	}

	// Check no duplicates
	seen := make(map[string]int)
	for _, db := range app1.SupDB {
		seen[db]++
	}
	for db, count := range seen {
		if count > 1 {
			t.Errorf("duplicate entry %q in sup_db", db)
		}
	}
}

func TestMergePrograms_MergeServer(t *testing.T) {
	t.Parallel()
	fileProgs := config.ProgramsFile{
		{Name: "app1", Server: map[string]string{"prod": "hyperion"}},
	}

	cfgProgs := []config.ProgramConfig{
		{Name: "app1", Server: map[string]string{"sham": "hyperion-sham"}},
	}

	fileProgs = config.MergePrograms(fileProgs, cfgProgs)

	if fileProgs[0].Server["sham"] != "hyperion-sham" {
		t.Errorf("expected sham server mapping, got %v", fileProgs[0].Server)
	}
	if fileProgs[0].Server["prod"] != "hyperion" {
		t.Errorf("expected prod server mapping to be preserved, got %v", fileProgs[0].Server)
	}
}

func TestMergePrograms_NewProgram(t *testing.T) {
	t.Parallel()
	fileProgs := config.ProgramsFile{
		{Name: "app1", AppDB: []string{"db1"}},
	}

	cfgProgs := []config.ProgramConfig{
		{Name: "app2", AppDB: []string{"db2"}},
	}

	fileProgs = config.MergePrograms(fileProgs, cfgProgs)

	if len(fileProgs) != 2 {
		t.Fatalf("expected 2 programs, got %d", len(fileProgs))
	}
}

func TestLoad(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Write config.yaml
	configYAML := `programs_file: "programs.yaml"
servers_file: "servers.yaml"
permission_sets:
  usage:
    - USAGE
  select:
    - SELECT
roles:
  - name: "ro"
    server:
      '*': [ "usage" ]
    app_db:
      '*': [ "select" ]
`
	os.WriteFile(filepath.Join(tmpDir, "config.yaml"), []byte(configYAML), 0o644)

	// Write programs.yaml
	programsYAML := `- name: "app1"
  server:
    prod: "hyperion"
  app_db:
    - "db_app1"
`
	os.WriteFile(filepath.Join(tmpDir, "programs.yaml"), []byte(programsYAML), 0o644)

	// Write servers.yaml
	serversYAML := `hyperion:
  host: "localhost"
  port: 3306
  user: "root"
  password: "test"
`
	os.WriteFile(filepath.Join(tmpDir, "servers.yaml"), []byte(serversYAML), 0o644)

	cfg, srvs, progs, err := config.Load(filepath.Join(tmpDir, "config.yaml"))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.ProgramsFile != "programs.yaml" {
		t.Errorf("expected programs.yaml, got %s", cfg.ProgramsFile)
	}
	if _, ok := srvs["hyperion"]; !ok {
		t.Error("expected hyperion server")
	}
	if len(progs) != 1 || progs[0].Name != "app1" {
		t.Errorf("expected 1 program app1, got %v", progs)
	}
	if len(cfg.Roles) != 1 || cfg.Roles[0].Name != "ro" {
		t.Errorf("expected 1 role ro, got %v", cfg.Roles)
	}
}

func TestValidate_MissingProgramsFile(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	configYAML := `servers_file: "servers.yaml"
roles:
  - name: "ro"
`
	os.WriteFile(filepath.Join(tmpDir, "config.yaml"), []byte(configYAML), 0o644)

	_, _, _, err := config.Load(filepath.Join(tmpDir, "config.yaml"))
	if err == nil {
		t.Fatal("expected error for missing programs_file")
	}
}

func TestValidate_DuplicateRoleName(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		ProgramsFile:   "programs.yaml",
		ServersFile:    "servers.yaml",
		PermissionSets: map[string][]string{"usage": {"USAGE"}},
		Roles: []config.RoleConfig{
			{Name: "ro", Server: map[string][]string{"*": {"usage"}}},
			{Name: "ro", Server: map[string][]string{"*": {"usage"}}},
		},
	}
	srvs := config.ServersFile{}
	progs := config.ProgramsFile{}

	err := config.Validate(".", cfg, srvs, progs)
	if err == nil {
		t.Fatal("expected error for duplicate role name")
	}
}

func TestServerConfig_EnabledDefault(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	serversYAML := `hyperion:
  host: "localhost"
  port: 3306
  user: "root"
  password: "test"
titan:
  host: "titan.example.com"
  port: 3306
  user: "root"
  password: "test"
  enabled: false
`
	os.WriteFile(filepath.Join(tmpDir, "servers.yaml"), []byte(serversYAML), 0o644)

	srvs, err := config.LoadServers(filepath.Join(tmpDir, "servers.yaml"))
	if err != nil {
		t.Fatalf("LoadServers failed: %v", err)
	}

	// hyperion: enabled not specified, should default to true
	if !srvs["hyperion"].Enabled.Get() {
		t.Error("expected hyperion.Enabled to default to true")
	}

	// titan: enabled explicitly set to false
	if srvs["titan"].Enabled.Get() {
		t.Error("expected titan.Enabled to be false")
	}
}

func TestServerConfig_EnabledExplicitTrue(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	serversYAML := `hyperion:
  host: "localhost"
  port: 3306
  user: "root"
  password: "test"
  enabled: true
`
	os.WriteFile(filepath.Join(tmpDir, "servers.yaml"), []byte(serversYAML), 0o644)

	srvs, err := config.LoadServers(filepath.Join(tmpDir, "servers.yaml"))
	if err != nil {
		t.Fatalf("LoadServers failed: %v", err)
	}

	if !srvs["hyperion"].Enabled.Get() {
		t.Error("expected hyperion.Enabled to be true")
	}
}

func TestIsTemplate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want bool
	}{
		{"tp-{{name}}-adm", true},
		{"tp-{{name}}-ro", true},
		{"ro", false},
		{"admin", false},
		{"{{name}}", true},
	}

	for _, tt := range tests {
		got := config.IsTemplate(tt.name)
		if got != tt.want {
			t.Errorf("IsTemplate(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestExpandRole(t *testing.T) {
	t.Parallel()
	role := config.RoleConfig{
		Name:   "tp-{{name}}-adm",
		Server: map[string][]string{"*": {"process"}},
		AppDB:  map[string][]string{"*": {"select"}},
	}

	expanded := config.ExpandRole(role, "myapp")
	if expanded.Name != "tp-myapp-adm" {
		t.Errorf("expected tp-myapp-adm, got %s", expanded.Name)
	}
	if len(expanded.Server) != 1 || len(expanded.Server["*"]) != 1 {
		t.Errorf("expected server perms to be preserved, got %v", expanded.Server)
	}
	if len(expanded.AppDB) != 1 {
		t.Errorf("expected app_db perms to be preserved, got %v", expanded.AppDB)
	}
}

func TestExpandRole_NonTemplate(t *testing.T) {
	t.Parallel()
	role := config.RoleConfig{
		Name:   "admin",
		Server: map[string][]string{"*": {"process"}},
	}

	expanded := config.ExpandRole(role, "myapp")
	if expanded.Name != "admin" {
		t.Errorf("expected admin (unchanged), got %s", expanded.Name)
	}
}

func TestExpandRolesForServer(t *testing.T) {
	t.Parallel()
	roles := []config.RoleConfig{
		{Name: "tp-{{name}}-adm", Server: map[string][]string{"*": {"process"}}},
		{Name: "tp-{{name}}-ro", Server: map[string][]string{"*": {"usage"}}},
		{Name: "global_admin", Server: map[string][]string{"*": {"all"}}},
	}

	progs := config.ProgramsFile{
		{Name: "app1", Server: map[string]string{"prod": "hyperion"}, AppDB: []string{"db1"}, SupDB: []string{"sup1"}},
		{Name: "app2", Server: map[string]string{"prod": "hyperion"}, AppDB: []string{"db2"}, SupDB: []string{"sup2"}},
		{
			Name:   "app3",
			Server: map[string]string{"sham": "hyperion-sham"},
			AppDB:  []string{"db3"},
			SupDB:  []string{"sup3"},
		},
	}

	expanded := config.ExpandRolesForServer(roles, progs, "hyperion", "prod")

	// Should have: tp-app1-adm, tp-app1-ro, tp-app2-adm, tp-app2-ro, global_admin = 5
	if len(expanded) != 5 {
		t.Fatalf("expected 5 expanded roles, got %d: %v", len(expanded), expanded)
	}

	names := make(map[string]struct{})
	for _, r := range expanded {
		names[r.Name] = struct{}{}
	}

	expected := []string{"tp-app1-adm", "tp-app1-ro", "tp-app2-adm", "tp-app2-ro", "global_admin"}
	for _, e := range expected {
		if _, ok := names[e]; !ok {
			t.Errorf("expected role %q in expanded list", e)
		}
	}
}

func TestFilterPrograms_Allowlist(t *testing.T) {
	t.Parallel()
	progs := config.ProgramsFile{
		{Name: "app1", AppDB: []string{"db1"}},
		{Name: "app2", AppDB: []string{"db2"}},
		{Name: "app3", AppDB: []string{"db3"}},
	}

	filtered := config.FilterPrograms(progs, []string{"app1", "app3"})
	if len(filtered) != 2 {
		t.Fatalf("expected 2 programs, got %d", len(filtered))
	}
	names := make(map[string]struct{})
	for _, p := range filtered {
		names[p.Name] = struct{}{}
	}
	if _, ok := names["app1"]; !ok {
		t.Error("expected app1 in filtered list")
	}
	if _, ok := names["app3"]; !ok {
		t.Error("expected app3 in filtered list")
	}
	if _, ok := names["app2"]; ok {
		t.Error("app2 should not be in filtered list")
	}
}

func TestFilterPrograms_EmptyAllowlist(t *testing.T) {
	t.Parallel()
	progs := config.ProgramsFile{
		{Name: "app1", AppDB: []string{"db1"}},
		{Name: "app2", AppDB: []string{"db2"}},
	}

	filtered := config.FilterPrograms(progs, nil)
	if len(filtered) != 2 {
		t.Errorf("empty allowlist should return all programs, got %d", len(filtered))
	}
}

func TestFilterPrograms_NoMatch(t *testing.T) {
	t.Parallel()
	progs := config.ProgramsFile{
		{Name: "app1", AppDB: []string{"db1"}},
	}

	filtered := config.FilterPrograms(progs, []string{"nonexistent"})
	if len(filtered) != 0 {
		t.Errorf("no matching programs should return empty, got %d", len(filtered))
	}
}

func TestIgnoreErrorsConfig_BoolTrue(t *testing.T) {
	t.Parallel()
	var c config.IgnoreErrorsConfig
	err := yaml.Unmarshal([]byte("true"), &c)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !c.All {
		t.Error("expected All=true")
	}
	if !c.ShouldIgnore("table_not_found") {
		t.Error("ShouldIgnore should return true when All=true")
	}
	if !c.ShouldIgnore("anything") {
		t.Error("ShouldIgnore should return true for any error when All=true")
	}
}

func TestIgnoreErrorsConfig_List(t *testing.T) {
	t.Parallel()
	var c config.IgnoreErrorsConfig
	err := yaml.Unmarshal([]byte(`["table_not_found", "role_not_found"]`), &c)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.All {
		t.Error("expected All=false for list form")
	}
	if !c.ShouldIgnore("table_not_found") {
		t.Error("ShouldIgnore should return true for listed error")
	}
	if !c.ShouldIgnore("role_not_found") {
		t.Error("ShouldIgnore should return true for listed error")
	}
	if c.ShouldIgnore("access_denied") {
		t.Error("ShouldIgnore should return false for unlisted error")
	}
}

func TestIgnoreErrorsConfig_ListWithAll(t *testing.T) {
	t.Parallel()
	var c config.IgnoreErrorsConfig
	err := yaml.Unmarshal([]byte(`["all"]`), &c)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !c.ShouldIgnore("table_not_found") {
		t.Error("'all' in list should match any error type")
	}
}

func TestIgnoreErrorsConfig_Invalid(t *testing.T) {
	t.Parallel()
	var c config.IgnoreErrorsConfig
	err := yaml.Unmarshal([]byte(`{key: val}`), &c)
	if err == nil {
		t.Error("expected error for invalid ignore_errors value")
	}
}

func TestIgnoreErrorsConfig_SingleString(t *testing.T) {
	t.Parallel()
	var c config.IgnoreErrorsConfig
	err := yaml.Unmarshal([]byte(`"table_not_found"`), &c)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.All {
		t.Error("expected All=false for single string")
	}
	if !c.ShouldIgnore("table_not_found") {
		t.Error("ShouldIgnore should return true for the single listed error")
	}
	if c.ShouldIgnore("role_not_found") {
		t.Error("ShouldIgnore should return false for unlisted error")
	}
}

func TestIgnoreErrorsConfig_Nil(t *testing.T) {
	t.Parallel()
	var c *config.IgnoreErrorsConfig
	if c.ShouldIgnore("table_not_found") {
		t.Error("nil ShouldIgnore should return false")
	}
}

func TestLoad_ProgramEnabledMerge(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Programs file: trslot has no enabled field, app1 doesn't either
	programsYAML := `- name: app1
  server:
    prod: hyperion
  app_db:
    - zban_app1
- name: trslot
  server:
    prod: hyperion
  app_db:
    - zban_trslot
`
	os.WriteFile(filepath.Join(tmpDir, "programs.yaml"), []byte(programsYAML), 0o644)

	// Config overrides trslot with enabled: false
	configYAML := fmt.Sprintf(`---
programs_file: "%s/programs.yaml"
programs:
  - name: trslot
    enabled: false
permission_sets:
  usage: [USAGE]
roles:
  - name: tp-{{name}}-ro
    server:
      '*': [usage]
    app_db:
      '*': [usage]
`, tmpDir)

	os.WriteFile(filepath.Join(tmpDir, "config.yaml"), []byte(configYAML), 0o644)

	_, _, progs, err := config.Load(filepath.Join(tmpDir, "config.yaml"))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// After Load, trslot should have enabled=false
	found := false
	for _, p := range progs {
		if p.Name == "trslot" {
			found = true
			if p.Enabled.Get() {
				t.Error("trslot should have enabled=false after merge")
			}
		}
	}
	if !found {
		t.Fatal("trslot program not found in loaded programs")
	}

	// FilterDisabledPrograms should remove trslot
	filtered := config.FilterDisabledPrograms(progs)
	for _, p := range filtered {
		if p.Name == "trslot" {
			t.Error("trslot should be filtered out by FilterDisabledPrograms")
		}
	}
}

func TestProgramConfig_EnabledDefault(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	programsYAML := "- name: active\n  server:\n    prod: hyperion\n- name: disabled\n  server:\n    prod: hyperion\n  enabled: false\n"
	os.WriteFile(filepath.Join(tmpDir, "programs.yaml"), []byte(programsYAML), 0o644)

	progs, err := config.LoadProgramsFile(filepath.Join(tmpDir, "programs.yaml"))
	if err != nil {
		t.Fatalf("LoadProgramsFile failed: %v", err)
	}
	config.SetProgramDefaults(progs)

	if !progs[0].Enabled.Get() {
		t.Error("expected 'active' program Enabled to default to true")
	}
	if progs[1].Enabled.Get() {
		t.Error("expected 'disabled' program Enabled to be false")
	}
}

func TestFilterDisabledPrograms(t *testing.T) {
	t.Parallel()
	progs := config.ProgramsFile{
		{Name: "active", Server: map[string]string{"prod": "hyperion"}},
		{Name: "disabled", Server: map[string]string{"prod": "hyperion"}},
	}
	// Set enabled: true on first, false on second
	progs[0].Enabled.Set(true)
	progs[1].Enabled.Set(false)

	filtered := config.FilterDisabledPrograms(progs)
	if len(filtered) != 1 {
		t.Fatalf("expected 1 active program, got %d", len(filtered))
	}
	if filtered[0].Name != "active" {
		t.Errorf("expected 'active', got %q", filtered[0].Name)
	}
}

func TestProgramsFile_MapFormat(t *testing.T) {
	t.Parallel()
	yamlData := `foobar:
  server:
    prod: hyperion
  app_db:
    - zban_foobar
  sup_db:
    - foobar
`
	var progs config.ProgramsFile
	if err := yaml.Unmarshal([]byte(yamlData), &progs); err != nil {
		t.Fatalf("unmarshal map format: %v", err)
	}
	if len(progs) != 1 {
		t.Fatalf("expected 1 program, got %d", len(progs))
	}
	if progs[0].Name != "foobar" {
		t.Errorf("expected name foobar, got %q", progs[0].Name)
	}
	if len(progs[0].AppDB) != 1 || progs[0].AppDB[0] != "zban_foobar" {
		t.Errorf("expected app_db [zban_foobar], got %v", progs[0].AppDB)
	}
	if progs[0].Server["prod"] != "hyperion" {
		t.Errorf("expected server.prod=hyperion, got %v", progs[0].Server)
	}
}

func TestProgramsFile_ListFormat(t *testing.T) {
	t.Parallel()
	yamlData := `- name: foobar
  server:
    prod: hyperion
  app_db:
    - zban_foobar
  sup_db:
    - foobar
`
	var progs config.ProgramsFile
	if err := yaml.Unmarshal([]byte(yamlData), &progs); err != nil {
		t.Fatalf("unmarshal list format: %v", err)
	}
	if len(progs) != 1 {
		t.Fatalf("expected 1 program, got %d", len(progs))
	}
	if progs[0].Name != "foobar" {
		t.Errorf("expected name foobar, got %q", progs[0].Name)
	}
}

func TestProgramsFile_MapFormatWithEnabled(t *testing.T) {
	t.Parallel()
	yamlData := `trslot:
  server:
    prod: hyperion
  app_db:
    - zban_trslot
  enabled: false
active:
  server:
    prod: hyperion
  app_db:
    - zban_active
`
	var progs config.ProgramsFile
	if err := yaml.Unmarshal([]byte(yamlData), &progs); err != nil {
		t.Fatalf("unmarshal map format: %v", err)
	}
	config.SetProgramDefaults(progs)

	// Map format sorts by key, so "active" comes before "trslot"
	if len(progs) != 2 {
		t.Fatalf("expected 2 programs, got %d", len(progs))
	}
	if progs[0].Name != "active" {
		t.Errorf("expected first program 'active', got %q", progs[0].Name)
	}
	if progs[1].Name != "trslot" {
		t.Errorf("expected second program 'trslot', got %q", progs[1].Name)
	}
	if progs[1].Enabled.Get() {
		t.Error("trslot should have enabled=false")
	}
}
