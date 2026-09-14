package config_test

import (
	"errors"
	"testing"

	driver "github.com/go-sql-driver/mysql"
	"github.com/na4ma4/mysql-role-reconciler/internal/config"
)

func TestClassifyError_KnownCodes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		code uint16
		want string
	}{
		{1146, "table_not_found"},
		{1049, "schema_not_found"},
		{1394, "role_not_found"},
		{1396, "duplicate_role"},
		{1305, "routine_not_found"},
		{1045, "access_denied"},
		{1054, "column_not_found"},
		{1050, "already_exists"},
		{1062, "duplicate_entry"},
	}
	for _, tt := range tests {
		err := &driver.MySQLError{Number: tt.code, Message: "test"}
		got := config.ClassifyError(err)
		if got.String() != tt.want {
			t.Errorf("ClassifyError(mysql %d) = %q, want %q", tt.code, got, tt.want)
		}
	}
}

func TestIgnoreErrors_RoutineAliases(t *testing.T) {
	t.Parallel()

	tests := []string{"routine_not_found", "procedure_not_found"}
	for _, configured := range tests {
		for _, actual := range tests {
			var cfg config.IgnoreErrorsConfig
			cfg.Errors = []config.MySQLErrorCode{config.MySQLErrorCode(configured)}
			if !cfg.ShouldIgnore(config.MySQLErrorCode(actual)) {
				t.Errorf("configured %q should match actual %q", configured, actual)
			}
		}
	}
}

func TestClassifyError_UnknownCode(t *testing.T) {
	t.Parallel()
	err := &driver.MySQLError{Number: 9999, Message: "test"}
	got := config.ClassifyError(err)
	if got.String() != "mysql_9999" {
		t.Errorf("ClassifyError(unknown code) = %q, want %q", got, "mysql_9999")
	}
}

func TestClassifyError_NonMySQLError(t *testing.T) {
	t.Parallel()
	got := config.ClassifyError(errors.New("some go error"))
	if got.String() != "unknown" {
		t.Errorf("ClassifyError(non-MySQL) = %q, want %q", got, "unknown")
	}
}
