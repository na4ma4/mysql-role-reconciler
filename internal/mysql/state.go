package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"

	"github.com/na4ma4/mysql-role-reconciler/internal/config"
)

// Grant represents a parsed MySQL grant.
type Grant struct {
	ID          string // unique identifier for this grant, e.g., "myserver.mydb.*.myrole"
	Server      string // server this grant applies to
	Role        string
	Database    string // schema name; "*" means server-level (*.*)
	Table       string // table name; "*" means all tables (schema.*); empty for server-level
	Grants      []string
	GrantOption bool
}

// ActualState represents the current roles and grants on a server.
type ActualState struct {
	Roles  []string
	Grants []Grant
}

// ReadActualState queries the server for existing roles and their grants.
// Grants for all existing roles are fetched concurrently.
func ReadActualState(
	ctx context.Context,
	db *sql.DB,
	srvCfg config.ServerConfig,
	roleNames []string,
) (*ActualState, error) {
	var existingRoles []string
	{
		var err error
		existingRoles, err = queryExistingRoles(ctx, db, srvCfg, roleNames)
		if err != nil {
			return nil, fmt.Errorf("querying existing roles: %w", err)
		}
	}

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		first  error
		result []Grant
		wgPool chan struct{}
	)

	wgPool = make(chan struct{}, srvCfg.OpenConnections.Get())

	for _, role := range existingRoles {
		wg.Add(1)
		go func(r string) {
			wgPool <- struct{}{}
			defer func() { <-wgPool }()
			defer wg.Done()

			roleGrants, err := queryGrants(ctx, db, srvCfg, r)
			mu.Lock()
			if err != nil && first == nil {
				first = fmt.Errorf("querying grants for role %q: %w", r, err)
			}
			result = append(result, roleGrants...)
			mu.Unlock()
		}(role)
	}
	wg.Wait()

	if first != nil {
		return nil, first
	}

	return &ActualState{
		Roles:  existingRoles,
		Grants: result,
	}, nil
}

func queryExistingRoles(ctx context.Context, db *sql.DB, _ config.ServerConfig, roleNames []string) ([]string, error) {
	if len(roleNames) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(roleNames))
	args := make([]any, len(roleNames))
	for i, r := range roleNames {
		placeholders[i] = "?"
		args[i] = r
	}

	//nolint:gosec // The role names are placeholders, it's using parameters.
	query := fmt.Sprintf(
		"SELECT User FROM mysql.user WHERE User IN (%s) AND Host = '%%'",
		strings.Join(placeholders, ","),
	)

	var rows *sql.Rows
	{
		var err error
		rows, err = db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
	}
	defer rows.Close()

	var roles []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		roles = append(roles, name)
	}

	return roles, rows.Err()
}

func queryGrants(ctx context.Context, db *sql.DB, srvCfg config.ServerConfig, role string) ([]Grant, error) {
	// SHOW GRANTS does not support parameterized queries for account names.
	// The role name comes from our validated config, not user input.
	escaped := strings.ReplaceAll(role, "'", "''")
	query := fmt.Sprintf("SHOW GRANTS FOR '%s'@'%%'", escaped)

	var rows *sql.Rows
	{
		var err error
		rows, err = db.QueryContext(ctx, query)
		if err != nil {
			return nil, err
		}
	}
	defer rows.Close()

	var grants []Grant
	for rows.Next() {
		var grantStr string
		if err := rows.Scan(&grantStr); err != nil {
			return nil, err
		}
		parsed := ParseGrantString(ctx, db, srvCfg, role, grantStr)
		if parsed != nil {
			grants = append(grants, *parsed)
		}
	}

	return grants, rows.Err()
}

// QueryDatabases returns the list of database (schema) names on the server.
func QueryDatabases(ctx context.Context, db *sql.DB) ([]string, error) {
	var rows *sql.Rows
	{
		var err error
		rows, err = db.QueryContext(ctx, "SELECT SCHEMA_NAME FROM information_schema.SCHEMATA")
		if err != nil {
			return nil, fmt.Errorf("querying databases: %w", err)
		}
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// ParseGrantString parses a MySQL GRANT statement into a Grant struct.
//
// E.g., "GRANT SELECT, INSERT ON `mydb`.* TO 'myrole'@'%'"".
//
//nolint:mnd // grant parsing
func ParseGrantString(_ context.Context, _ *sql.DB, srvCfg config.ServerConfig, role, grantStr string) *Grant {
	// GRANT SELECT, INSERT ON `mydb`.* TO 'myrole'@'%'
	// GRANT USAGE ON *.* TO 'myrole'@'%'
	// GRANT ALL PRIVILEGES ON *.* TO 'myrole'@'%' WITH GRANT OPTION

	if !strings.HasPrefix(grantStr, "GRANT ") {
		return nil
	}

	rest := strings.TrimPrefix(grantStr, "GRANT ")

	// Split on " ON " to separate privileges from the target
	parts := strings.SplitN(rest, " ON ", 2)
	if len(parts) != 2 {
		return nil
	}

	privStr := strings.TrimSpace(parts[0])
	targetStr := strings.TrimSpace(parts[1])

	// Remove " TO 'role'@'%'" [WITH GRANT OPTION] suffix
	toIdx := strings.Index(targetStr, " TO ")
	if toIdx < 0 {
		return nil
	}
	dbPart := strings.TrimSpace(targetStr[:toIdx])
	suffixPart := targetStr[toIdx:]

	grantOption := strings.Contains(suffixPart, "WITH GRANT OPTION")

	// Parse database and table from target like `mydb`.`table`, `mydb`.*, or *.*
	database, table := ParseTargetFromGrant(dbPart)

	// Parse privileges
	privs := ParsePrivileges(privStr)

	return &Grant{
		ID:          strings.Join([]string{srvCfg.ID(), database, table, role}, "."), // e.g., "myserver.mydb.*.myrole"
		Role:        role,
		Server:      srvCfg.ID(),
		Database:    database,
		Table:       table,
		Grants:      privs,
		GrantOption: grantOption,
	}
}

// ParseTargetFromGrant parses the database and table from a grant target.
// *.* → ("*", "")  (server-level)
// `mydb`.* → ("mydb", "*")  (all tables in schema)
// `mydb`.`mytable` → ("mydb", "mytable")  (specific table)
//
//nolint:mnd // db and table parts
func ParseTargetFromGrant(target string) (string, string) {
	target = strings.TrimSpace(target)

	if target == "*.*" {
		return "*", ""
	}

	// `mydb`.*
	if before, ok := strings.CutSuffix(target, ".*"); ok {
		dbPart := before
		dbPart = strings.Trim(dbPart, "`")
		return dbPart, "*"
	}

	// `mydb`.`mytable`
	parts := strings.SplitN(target, "`.`", 2)
	if len(parts) == 2 {
		dbPart := strings.Trim(parts[0], "`")
		tblPart := strings.Trim(parts[1], "`")
		return dbPart, tblPart
	}

	// Fallback: single backtick-quoted identifier
	return strings.Trim(target, "`"), "*"
}

func ParsePrivileges(privStr string) []string {
	if privStr == "ALL PRIVILEGES" {
		return []string{"ALL PRIVILEGES"}
	}

	parts := strings.Split(privStr, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}
