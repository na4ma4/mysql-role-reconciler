package config

import (
	"errors"
	"fmt"

	driver "github.com/go-sql-driver/mysql"
)

type MySQLErrorCode string

func (c MySQLErrorCode) String() string {
	return string(c)
}

const (
	MySQLErrorAccessDenied   MySQLErrorCode = "access_denied"
	MySQLErrorSchemaNotFound MySQLErrorCode = "schema_not_found"
	MySQLErrorAlreadyExists  MySQLErrorCode = "already_exists"
	MySQLErrorColumnNotFound MySQLErrorCode = "column_not_found"
	MySQLErrorDuplicateEntry MySQLErrorCode = "duplicate_entry"
	MySQLErrorTableNotFound  MySQLErrorCode = "table_not_found"
	MySQLErrorRoleNotFound   MySQLErrorCode = "role_not_found"
	MySQLErrorDuplicateRole  MySQLErrorCode = "duplicate_role"
)

// Named error types that can be used in program ignore_errors config.
// Maps MySQL error codes to short human-friendly names.
//
//nolint:gochecknoglobals // (this is a static map of constants, not mutable state)
var mySQLErrorNames = map[uint16]MySQLErrorCode{
	1045: MySQLErrorAccessDenied,
	1049: MySQLErrorSchemaNotFound,
	1050: MySQLErrorAlreadyExists,
	1054: MySQLErrorColumnNotFound,
	1062: MySQLErrorDuplicateEntry,
	1146: MySQLErrorTableNotFound,
	1394: MySQLErrorRoleNotFound,
	1396: MySQLErrorDuplicateRole,
}

// ClassifyError returns a short name for a MySQL error, or "unknown" if
// the error is not a MySQL error. Known names match the keys defined in
// the ignore_errors config. Unrecognized MySQL errors return "mysql_<code>".
func ClassifyError(err error) MySQLErrorCode {
	var mysqlErr *driver.MySQLError
	if errors.As(err, &mysqlErr) {
		if name, ok := mySQLErrorNames[mysqlErr.Number]; ok {
			return name
		}
		return MySQLErrorCode(fmt.Sprintf("mysql_%d", mysqlErr.Number))
	}
	return MySQLErrorCode("unknown")
}
