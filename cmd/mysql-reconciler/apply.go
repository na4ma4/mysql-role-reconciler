package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/na4ma4/mysql-role-reconciler/internal/config"
	"github.com/na4ma4/mysql-role-reconciler/internal/migrate"
	mysqlclient "github.com/na4ma4/mysql-role-reconciler/internal/mysql"
	"github.com/na4ma4/mysql-role-reconciler/internal/reconcile"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var applyCmd = &cobra.Command{
	Use:   "apply -c config.yaml PLAN_FILE",
	Short: "Apply migration plan to servers",
	Long:  `Execute the migration statements from a plan file against the target servers and record history.`,
	Args:  cobra.ExactArgs(1),
	RunE:  runApply,
}

func init() {
	applyCmd.Flags().
		StringP("environment", "e", "", "Environment name (defaults to the environment stored in the plan file)")

	rootCmd.AddCommand(applyCmd)
}

// interrupted is set to 1 when SIGINT is received during apply.
// The statement loop checks this on each iteration and breaks if set,
// entering the same partial-apply save path as a MySQL error.
var interrupted atomic.Int32

func runApply(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true

	configPath := viper.GetString("config")
	envFlag, _ := cmd.Flags().GetString("environment")
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

	// Skip programs with enabled: false (needed for ignore_errors lookup)
	progs = config.FilterDisabledPrograms(progs)

	var plan *migrate.PlanFile
	{
		var err error
		plan, err = migrate.ReadPlanFile(planPath)
		if err != nil {
			return fmt.Errorf("reading plan file: %w", err)
		}
	}

	env := plan.Environment
	if envFlag != "" && envFlag != env {
		return fmt.Errorf("plan environment %q does not match requested environment %q", plan.Environment, envFlag)
	}

	ctx := context.Background()

	var store migrate.Storage
	{
		var err error
		store, err = storageFromConfig(ctx, cfg)
		if err != nil {
			return fmt.Errorf("creating storage: %w", err)
		}
	}

	var stateStore *migrate.StateStore
	{
		var err error
		stateStore, err = migrate.LoadStateStore(ctx, store)
		if err != nil {
			return fmt.Errorf("loading state store: %w", err)
		}
	}

	// Validate that state hasn't changed since the plan was generated.
	if err := validatePlanState(plan, stateStore); err != nil {
		return err
	}

	// Install SIGINT handler for graceful cancellation.
	sigCh := setupSignalHandler()

	cmd.SilenceUsage = true

	applyErr := applyAllServers(ctx, plan, srvs, env, store, stateStore, cfg, progs)

	// Stop listening for SIGINT; restore default behavior.
	signal.Stop(sigCh)
	close(sigCh)

	if applyErr != nil {
		return applyErr
	}

	if interrupted.Load() == 1 {
		return errors.New("apply cancelled by interrupt")
	}

	fmt.Fprintln(os.Stdout, "Apply complete.")
	return nil
}

func applyServer(
	ctx context.Context,
	sp migrate.ServerPlan,
	srvs map[string]config.ServerConfig,
	env string,
	store migrate.Storage,
	stateStore *migrate.StateStore,
	cfg *config.Config,
	progs config.ProgramsFile,
) error {
	srvCfg, ok := srvs[sp.Server]
	if !ok {
		return fmt.Errorf("server %q not found in servers config", sp.Server)
	}

	if !srvCfg.Enabled.Get() {
		fmt.Fprintf(os.Stdout, "# Server %q: disabled, skipping\n", sp.Server)
		return nil
	}

	if len(sp.Statements) == 0 {
		fmt.Fprintf(os.Stdout, "# Server %q: no statements to apply\n", sp.Server)
		return nil
	}

	var db *sql.DB
	{
		var err error
		db, err = mysqlclient.Connect(ctx, srvCfg)
		if err != nil {
			return fmt.Errorf("connecting to server %q: %w", sp.Server, err)
		}
	}

	// Build role → program name mapping so we can look up ignore_errors per statement.
	programDBs := config.BuildProgramDBMap(sp.Server, env, progs)
	roleProgMap := config.BuildRoleProgramMap(cfg.Roles, programDBs)
	progIgnoreMap := buildProgIgnoreMap(progs)

	// Compute the full desired state for the state store update after apply.
	// The plan file only contains state for changed roles/grants, so we rebuild
	// the full desired state from config + the server's database list.
	desired := computeDesiredState(ctx, db, sp.Server, env, cfg, progs)

	fmt.Fprintf(os.Stdout, "# Applying %d statement(s) to server %q\n", len(sp.Statements), sp.Server)

	var appliedStatements []string
	var applyErr error
	for _, stmt := range sp.Statements {
		// Check for interrupt before executing the next statement.
		if interrupted.Load() == 1 {
			applyErr = fmt.Errorf("server %q: apply interrupted by signal", sp.Server)
			fmt.Fprintf(os.Stderr, "  ! INTERRUPTED\n")
			break
		}

		if viper.GetBool("debug") {
			fmt.Fprintf(os.Stderr, "  Executing: %s\n", stmt.SQL)
		}

		if err := executeStatement(ctx, db, stmt.SQL); err != nil {
			errType := config.ClassifyError(err)
			progName := roleProgMap[stmt.Role]
			ignore := progIgnoreMap[progName]

			if ignore.ShouldIgnore(errType) {
				fmt.Fprintf(
					os.Stderr,
					"  ~ IGNORED [%s]: %s (program %q ignores %q)\n",
					errType,
					stmt.SQL,
					progName,
					errType,
				)
				continue
			}

			applyErr = fmt.Errorf("executing on %q: %q: %w", sp.Server, stmt.SQL, err)
			fmt.Fprintf(os.Stderr, "  ! ERROR [%s]: %s\n", errType, stmt.SQL)
			break
		}

		fmt.Fprintf(os.Stdout, "  + %s\n", stmt.SQL)
		appliedStatements = append(appliedStatements, stmt.SQL)
	}

	if applyErr != nil {
		return savePartialApply(ctx, db, sp, env, appliedStatements, applyErr, store, stateStore)
	}

	_ = db.Close()

	if err := completeServerApply(ctx, sp, env, appliedStatements, store, stateStore, desired); err != nil {
		return err
	}

	fmt.Fprintf(os.Stdout, "# Server %q: migration complete\n", sp.Server)
	return nil
}

// completeServerApply writes the history entry and updates the state store after a successful apply.
func completeServerApply(
	ctx context.Context,
	sp migrate.ServerPlan,
	env string,
	appliedStatements []string,
	store migrate.Storage,
	stateStore *migrate.StateStore,
	desired *reconcile.DesiredState,
) error {
	now := time.Now().UTC().Format(time.RFC3339)

	if err := migrate.WriteHistory(ctx, store, migrate.HistoryEntry{
		Timestamp:   now,
		Environment: env,
		Server:      sp.Server,
		Statements:  appliedStatements,
		Checksum:    sp.Checksum,
	}); err != nil {
		return fmt.Errorf("writing history: %w", err)
	}

	if err := stateStore.Update(ctx, sp.Server, migrate.ServerState{
		AppliedAt:   now,
		Environment: env,
		Checksum:    sp.Checksum,
		Roles:       desired.Roles,
		Grants:      desiredGrantsToMigrateEntries(desired.Grants),
	}); err != nil {
		return fmt.Errorf("updating state store: %w", err)
	}

	return nil
}

// desiredGrantsToMigrateEntries converts reconcile.DesiredGrant to migrate.GrantEntry.
func desiredGrantsToMigrateEntries(grants []reconcile.DesiredGrant) []migrate.GrantEntry {
	entries := make([]migrate.GrantEntry, len(grants))
	for i, g := range grants {
		entries[i] = migrate.GrantEntry{
			Role:       g.Role,
			Database:   g.Database,
			Table:      g.Table,
			Privileges: g.Privileges,
		}
	}
	return entries
}

// buildProgIgnoreMap creates a program name → IgnoreErrorsConfig lookup.
func buildProgIgnoreMap(progs config.ProgramsFile) map[string]*config.IgnoreErrorsConfig {
	m := make(map[string]*config.IgnoreErrorsConfig, len(progs))
	for i := range progs {
		ie := &progs[i].IgnoreErrors
		if ie.All || len(ie.Errors) > 0 {
			m[progs[i].Name] = ie
		}
	}
	return m
}

// savePartialApply marks the state as stale, records a partial history entry,
// closes the database connection, and returns the original error.
// The stale marker changes the state checksum, forcing a re-plan before the next apply.
func savePartialApply(
	ctx context.Context,
	db *sql.DB,
	sp migrate.ServerPlan,
	env string,
	appliedStatements []string,
	applyErr error,
	store migrate.Storage,
	stateStore *migrate.StateStore,
) error {
	now := time.Now().UTC().Format(time.RFC3339)
	partialChecksum := migrate.ComputeChecksumFromSQL(appliedStatements)

	fmt.Fprintln(os.Stderr, "# Saving partial state and history…")

	_ = db.Close()

	// Mark the state as stale so the changed checksum forces a re-plan.
	if markErr := stateStore.MarkStale(ctx, sp.Server, env, applyErr.Error()); markErr != nil {
		return fmt.Errorf(
			"partial apply on %q: marking state stale failed: %w (original error: %w)",
			sp.Server,
			markErr,
			applyErr,
		)
	}

	// Determine the failed/interrupted SQL for the history entry.
	failedSQL := ""
	if idx := len(appliedStatements); idx < len(sp.Statements) {
		failedSQL = sp.Statements[idx].SQL
	}

	histErr := migrate.WriteHistory(ctx, store, migrate.HistoryEntry{
		Timestamp:   now,
		Environment: env,
		Server:      sp.Server,
		Statements:  appliedStatements,
		Checksum:    partialChecksum,
		Error:       applyErr.Error(),
		FailedSQL:   failedSQL,
	})
	if histErr != nil {
		return fmt.Errorf(
			"partial apply on %q failed, and writing history also failed: %w (original error: %w)",
			sp.Server,
			histErr,
			applyErr,
		)
	}

	fmt.Fprintf(
		os.Stderr,
		"# Server %q: state marked stale, partial apply recorded (%d/%d statements applied)\n",
		sp.Server,
		len(appliedStatements),
		len(sp.Statements),
	)
	return applyErr
}

// setupSignalHandler installs a SIGINT handler for graceful cancellation during apply.
// The first Ctrl+C sets the interrupted flag; the second forces an immediate exit.
func setupSignalHandler() chan os.Signal {
	interrupted.Store(0)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT)
	go func() {
		first := true
		for range sigCh {
			if first {
				interrupted.Store(1)
				fmt.Fprintln(
					os.Stderr,
					"\n# Interrupt received, cleaning up after current statement… press Ctrl+C again to abort (this will lead to data loss)",
				)
				first = false
			} else {
				fmt.Fprintln(os.Stderr, "\n# Aborting — state and history may be incomplete!")
				os.Exit(exitCodeInterrupt)
			}
		}
	}()
	return sigCh
}

// applyAllServers iterates over the plan's server entries, validates each,
// and applies statements. Returns the first error encountered.
func applyAllServers(
	ctx context.Context,
	plan *migrate.PlanFile,
	srvs config.ServersFile,
	env string,
	store migrate.Storage,
	stateStore *migrate.StateStore,
	cfg *config.Config,
	progs config.ProgramsFile,
) error {
	for _, sp := range plan.Servers {
		if err := sp.Validate(); err != nil {
			return fmt.Errorf("validating server plan for %q: %w", sp.Server, err)
		}

		if err := applyServer(ctx, sp, srvs, env, store, stateStore, cfg, progs); err != nil {
			return err
		}
	}
	return nil
}

// validatePlanState checks that the state store checksum for each server
// matches the checksum recorded in the plan file at generation time.
func validatePlanState(plan *migrate.PlanFile, stateStore *migrate.StateStore) error {
	for _, sp := range plan.Servers {
		if sp.StateChecksum != "" {
			current := stateStore.ChecksumFor(sp.Server)
			if current != sp.StateChecksum {
				return fmt.Errorf(
					"server %q: %w (plan was generated against a different state — re-run plan)",
					sp.Server,
					migrate.ErrStateChanged,
				)
			}
		}
	}
	return nil
}

func executeStatement(ctx context.Context, db *sql.DB, sql string) error {
	_, err := db.ExecContext(ctx, sql)
	return err
}

// computeDesiredState builds the full desired state from config and the server's
// database list. This is needed during apply because the plan file only contains
// state for changed roles/grants, but the state store requires the full desired state.
func computeDesiredState(
	ctx context.Context,
	db *sql.DB,
	srvName, env string,
	cfg *config.Config,
	progs config.ProgramsFile,
) *reconcile.DesiredState {
	expandedRoles := config.ExpandRolesForServer(cfg.Roles, progs, srvName, env)
	programDBs := config.BuildProgramDBMap(srvName, env, progs)
	desired := reconcile.BuildDesiredStateFromExpanded(
		srvName, cfg.Roles, expandedRoles, programDBs, cfg.PermissionSets,
	)

	dbNames, err := mysqlclient.QueryDatabases(ctx, db)
	if err == nil {
		reconcile.ExpandDatabasePatterns(desired, dbNames)
	}

	return desired
}
