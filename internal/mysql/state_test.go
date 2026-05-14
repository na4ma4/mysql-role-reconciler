package mysql_test

import (
	"testing"

	"github.com/na4ma4/mysql-role-reconciler/internal/config"
	"github.com/na4ma4/mysql-role-reconciler/internal/mysql"
)

func TestParseGrantString_ServerLevel(t *testing.T) {
	t.Parallel()
	result := mysql.ParseGrantString(
		t.Context(),
		nil,
		config.ServerConfig{Host: "test-host.example.com"},
		"ro",
		"GRANT USAGE ON *.* TO 'ro'@'%'",
	)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Database != "*" {
		t.Errorf("expected database '*', got %q", result.Database)
	}
	if result.Table != "" {
		t.Errorf("expected empty table for server-level, got %q", result.Table)
	}
	if len(result.Grants) != 1 || result.Grants[0] != "USAGE" {
		t.Errorf("expected [USAGE], got %v", result.Grants)
	}
}

func TestParseGrantString_DatabaseLevel(t *testing.T) {
	t.Parallel()
	result := mysql.ParseGrantString(
		t.Context(),
		nil,
		config.ServerConfig{Host: "test-host.example.com"},
		"ro",
		"GRANT SELECT, INSERT ON `mydb`.* TO 'ro'@'%'",
	)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Database != "mydb" {
		t.Errorf("expected database 'mydb', got %q", result.Database)
	}
	if result.Table != "*" {
		t.Errorf("expected table '*', got %q", result.Table)
	}
	if len(result.Grants) != 2 {
		t.Errorf("expected 2 grants, got %d: %v", len(result.Grants), result.Grants)
	}
}

func TestParseGrantString_TableLevel(t *testing.T) {
	t.Parallel()
	result := mysql.ParseGrantString(
		t.Context(),
		nil,
		config.ServerConfig{Host: "test-host.example.com"},
		"ro",
		"GRANT SELECT, INSERT ON `mydb`.`objects` TO 'ro'@'%'",
	)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Database != "mydb" {
		t.Errorf("expected database 'mydb', got %q", result.Database)
	}
	if result.Table != "objects" {
		t.Errorf("expected table 'objects', got %q", result.Table)
	}
	if len(result.Grants) != 2 {
		t.Errorf("expected 2 grants, got %d: %v", len(result.Grants), result.Grants)
	}
}

func TestParseGrantString_AllPrivileges(t *testing.T) {
	t.Parallel()
	result := mysql.ParseGrantString(
		t.Context(),
		nil,
		config.ServerConfig{Host: "test-host.example.com"},
		"adm",
		"GRANT ALL PRIVILEGES ON *.* TO 'adm'@'%' WITH GRANT OPTION",
	)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Database != "*" {
		t.Errorf("expected database '*', got %q", result.Database)
	}
	if len(result.Grants) != 1 || result.Grants[0] != "ALL PRIVILEGES" {
		t.Errorf("expected [ALL PRIVILEGES], got %v", result.Grants)
	}
	if !result.GrantOption {
		t.Error("expected GrantOption to be true")
	}
}

func TestParseGrantString_NonGrant(t *testing.T) {
	t.Parallel()
	result := mysql.ParseGrantString(
		t.Context(),
		nil,
		config.ServerConfig{Host: "test-host.example.com"},
		"ro",
		"IDENTIFIED BY PASSWORD 'xxx'",
	)
	if result != nil {
		t.Errorf("expected nil for non-GRANT string, got %v", result)
	}
}

func TestParseTargetFromGrant(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input     string
		wantDB    string
		wantTable string
	}{
		{"*.*", "*", ""},
		{"`mydb`.*", "mydb", "*"},
		{"`mydb`.`objects`", "mydb", "objects"},
		{"`zban_app`.`users`", "zban_app", "users"},
	}

	for _, tt := range tests {
		gotDB, gotTable := mysql.ParseTargetFromGrant(tt.input)
		if gotDB != tt.wantDB || gotTable != tt.wantTable {
			t.Errorf("ParseTargetFromGrant(%q) = (%q, %q), want (%q, %q)",
				tt.input, gotDB, gotTable, tt.wantDB, tt.wantTable)
		}
	}
}

func TestParsePrivileges(t *testing.T) {
	t.Parallel()
	result := mysql.ParsePrivileges("ALL PRIVILEGES")
	if len(result) != 1 || result[0] != "ALL PRIVILEGES" {
		t.Errorf("expected [ALL PRIVILEGES], got %v", result)
	}

	result = mysql.ParsePrivileges("SELECT, INSERT, DELETE")
	if len(result) != 3 {
		t.Errorf("expected 3 privileges, got %d: %v", len(result), result)
	}
}
