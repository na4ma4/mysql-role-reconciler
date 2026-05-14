package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/na4ma4/mysql-role-reconciler/internal/config"
	"github.com/na4ma4/mysql-role-reconciler/internal/migrate"
	mysqlclient "github.com/na4ma4/mysql-role-reconciler/internal/mysql"
	"github.com/na4ma4/mysql-role-reconciler/internal/reconcile"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var planCmd = &cobra.Command{
	Use:   "plan -c config.yaml -e environment PLAN_FILE",
	Short: "Generate migration plan (dry-run)",
	Long:  `Connect to servers, compare desired vs actual state, and write migration SQL to a plan file.`,
	Args:  cobra.ExactArgs(1),
	RunE:  runPlan,
}

func init() {
	planCmd.Flags().StringP("environment", "e", "", "Environment name (e.g., prod, sham) (required)")
	_ = planCmd.MarkFlagRequired("environment")

	planCmd.Flags().Bool("drop-roles", false, "Include DROP ROLE statements for roles not in config")

	planCmd.Flags().StringArrayP("program", "p", nil, "Target only the specified program(s); may be set multiple times")

	rootCmd.AddCommand(planCmd)
}

func runPlan(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true

	configPath := viper.GetString("config")
	env, _ := cmd.Flags().GetString("environment")
	dropRoles, _ := cmd.Flags().GetBool("drop-roles")
	programFilter, _ := cmd.Flags().GetStringArray("program")
	planPath := args[0]

	var (
		cfg   *config.Config
		srvs  config.ServersFile
		progs config.ProgramsFile
	)
	{
		var err error
		cfg, srvs, progs, err = config.Load(configPath)
		if err != nil {
			return fmt.Errorf("loading config: %w", err)
		}
	}

	// Filter programs if --program is specified
	progs = config.FilterPrograms(progs, programFilter)

	// Skip programs with enabled: false
	progs = config.FilterDisabledPrograms(progs)

	if err := config.ValidateProgramServers(progs, srvs, env); err != nil {
		return fmt.Errorf("validation: %w", err)
	}

	// Collect all server names referenced by programs for this environment
	serverNames := collectServers(env, progs, srvs)

	ctx := context.Background()

	var stateStore *migrate.StateStore
	{
		var err error
		store, err := storageFromConfig(ctx, cfg)
		if err != nil {
			return fmt.Errorf("creating storage: %w", err)
		}
		stateStore, err = migrate.LoadStateStore(ctx, store)
		if err != nil {
			return fmt.Errorf("loading state store: %w", err)
		}
	}

	plans, planErr := planAllServers(ctx, env, cfg, progs, srvs, serverNames, dropRoles)
	if planErr != nil {
		return planErr
	}

	// Attach state checksums so apply can detect stale plans
	for _, p := range plans {
		p.StateChecksum = stateStore.ChecksumFor(p.Server)
	}

	if writeErr := migrate.WritePlanFile(planPath, env, plans); writeErr != nil {
		return fmt.Errorf("writing plan file: %w", writeErr)
	}

	fmt.Fprintf(os.Stdout, "\nPlan written to %s\n", planPath)
	return nil
}

// planAllServers plans each server concurrently and returns the sorted results.
func planAllServers(
	ctx context.Context,
	env string,
	cfg *config.Config,
	progs config.ProgramsFile,
	srvs config.ServersFile,
	serverNames []string,
	dropRoles bool,
) ([]*reconcile.Plan, error) {
	type serverResult struct {
		index int
		plan  *reconcile.Plan
		err   error
	}

	var wg sync.WaitGroup
	results := make([]serverResult, len(serverNames))

	for i, srvName := range serverNames {
		srvCfg, ok := srvs[srvName]
		if !ok {
			return nil, fmt.Errorf("server %q not found in servers config", srvName)
		}

		if !srvCfg.Enabled.Get() {
			fmt.Fprintf(os.Stdout, "# Server %q: disabled, skipping\n", srvName)
			continue
		}

		wg.Add(1)
		go func(idx int, name string, cfg_ config.ServerConfig) {
			defer wg.Done()
			plan, err := planServer(ctx, env, name, cfg_, cfg, progs, dropRoles)
			results[idx] = serverResult{index: idx, plan: plan, err: err}
		}(i, srvName, srvCfg)
	}
	wg.Wait()

	plans := make([]*reconcile.Plan, 0, len(serverNames))
	for _, r := range results {
		if r.err != nil {
			return nil, r.err
		}
		if r.plan != nil {
			plans = append(plans, r.plan)
		}
	}

	// Sort plans by server name for deterministic output
	sort.Slice(plans, func(i, j int) bool {
		return plans[i].Server < plans[j].Server
	})

	return plans, nil
}

func collectServers(env string, progs config.ProgramsFile, _ config.ServersFile) []string {
	seen := make(map[string]struct{})
	var names []string

	for _, p := range progs {
		srvName, exists := p.Server[env]
		if !exists {
			continue
		}
		if _, seenAlready := seen[srvName]; !seenAlready {
			seen[srvName] = struct{}{}
			names = append(names, srvName)
		}
	}

	sort.Strings(names)
	return names
}

var ErrServerDisabled = errors.New("server is disabled")

func planServer(
	ctx context.Context,
	env string,
	srvName string,
	srvCfg config.ServerConfig,
	cfg *config.Config,
	progs config.ProgramsFile,
	dropRoles bool,
) (*reconcile.Plan, error) {
	if !srvCfg.Enabled.Get() {
		fmt.Fprintf(os.Stdout, "# Server %q: disabled, skipping\n", srvName)
		return nil, ErrServerDisabled
	}

	if viper.GetBool("debug") {
		fmt.Fprintf(os.Stderr, "Connecting to server %q...\n", srvName)
	}

	var db *sql.DB
	{
		var err error
		db, err = mysqlclient.Connect(ctx, srvCfg)
		if err != nil {
			return nil, fmt.Errorf("connecting to server %q: %w", srvName, err)
		}
	}

	// Expand role templates to get concrete role names for this server (computed once)
	expandedRoles := config.ExpandRolesForServer(cfg.Roles, progs, srvName, env)
	programDBs := config.BuildProgramDBMap(srvName, env, progs)
	roleNames := make([]string, len(expandedRoles))
	for i, r := range expandedRoles {
		roleNames[i] = r.Name
	}

	var actual *mysqlclient.ActualState
	{
		var err error
		actual, err = mysqlclient.ReadActualState(ctx, db, srvCfg, roleNames)
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("reading actual state from %q: %w", srvName, err)
		}
	}

	// Query database list for expanding LIKE-pattern database names (e.g., zban_qa%)
	var dbNames []string
	{
		var err error
		dbNames, err = mysqlclient.QueryDatabases(ctx, db)
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("querying databases from %q: %w", srvName, err)
		}
	}

	_ = db.Close()

	desired := reconcile.BuildDesiredStateFromExpanded(
		srvName,
		cfg.Roles,
		expandedRoles,
		programDBs,
		cfg.PermissionSets,
	)
	reconcile.ExpandDatabasePatterns(desired, dbNames)
	stmts := reconcile.Diff(desired, actual, dropRoles)
	checksum := migrate.ComputeChecksum(stmts)

	plan := &reconcile.Plan{
		Server:     srvName,
		Statements: stmts,
		Checksum:   checksum,
		Roles:      desired.Roles,
		Grants:     desired.Grants,
	}

	if len(stmts) == 0 {
		fmt.Fprintf(os.Stdout, "# Server %q: no changes needed\n", srvName)
	} else {
		fmt.Fprintf(os.Stdout, "# Server %q: %d statement(s) to apply\n", srvName, len(stmts))
		for _, s := range stmts {
			fmt.Fprintf(os.Stdout, "  %s\n", s.SQL)
		}
	}

	return plan, nil
}
