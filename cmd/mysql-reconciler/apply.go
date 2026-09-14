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
	applyCmd.Flags().Bool("warn-on-error", false, "Continue after individual SQL failures and treat them as warnings")

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
	warnOnError, _ := cmd.Flags().GetBool("warn-on-error")
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

	summary, applyErr := applyAllServers(ctx, plan, srvs, env, store, stateStore, cfg, progs, warnOnError)

	// Stop listening for SIGINT; restore default behavior.
	signal.Stop(sigCh)
	close(sigCh)

	if applyErr != nil {
		return applyErr
	}

	if interrupted.Load() == 1 {
		return errors.New("apply cancelled by interrupt")
	}

	if warnOnError && summary.FailedStatements > 0 {
		if summary.allAttemptedStatementsFailed() {
			return fmt.Errorf(
				"all %d attempted statement(s) failed across %d server(s)",
				summary.FailedStatements,
				summary.AffectedServers,
			)
		}
		fmt.Fprintf(
			os.Stdout,
			"Apply complete with warnings (%d statement(s) applied, %d failed).\n",
			summary.AppliedStatements,
			summary.FailedStatements,
		)
		return nil
	}

	fmt.Fprintln(os.Stdout, "Apply complete.")
	return nil
}

type applySummary struct {
	AppliedStatements int
	FailedStatements  int
	AffectedServers   int
}

func (s applySummary) allAttemptedStatementsFailed() bool {
	return s.FailedStatements > 0 && s.AppliedStatements == 0
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
	warnOnError bool,
) (applySummary, error) {
	srvCfg, ok := srvs[sp.Server]
	if !ok {
		return applySummary{}, fmt.Errorf("server %q not found in servers config", sp.Server)
	}

	if !srvCfg.Enabled.Get() {
		fmt.Fprintf(os.Stdout, "# Server %q: disabled, skipping\n", sp.Server)
		return applySummary{}, nil
	}

	if len(sp.Statements) == 0 {
		fmt.Fprintf(os.Stdout, "# Server %q: no statements to apply\n", sp.Server)
		return applySummary{}, nil
	}

	var db *sql.DB
	{
		var err error
		db, err = mysqlclient.Connect(ctx, srvCfg)
		if err != nil {
			return applySummary{}, fmt.Errorf("connecting to server %q: %w", sp.Server, err)
		}
	}

	// Build role → program name mapping so we can look up ignore_errors per statement.
	programDBs := config.BuildProgramDBMap(sp.Server, env, progs)
	roleProgMap := config.BuildRoleProgramMap(cfg.Roles, programDBs)
	progIgnoreMap := buildProgIgnoreMap(progs)
	serverIgnore := &srvCfg.IgnoreErrors

	// Compute the full desired state for the state store update after apply.
	// The plan file only contains state for changed roles/grants, so we rebuild
	// the full desired state from config + the server's database list.
	desired := computeDesiredState(ctx, db, sp.Server, env, cfg, progs)

	fmt.Fprintf(os.Stdout, "# Applying %d statement(s) to server %q\n", len(sp.Statements), sp.Server)

	appliedStatements, failedSQL, failedStatements, applyErr := applyServerStatements(
		ctx,
		db,
		sp,
		roleProgMap,
		progIgnoreMap,
		serverIgnore,
		warnOnError,
	)

	return finalizeServerApply(
		ctx,
		db,
		sp,
		env,
		appliedStatements,
		failedSQL,
		failedStatements,
		applyErr,
		store,
		stateStore,
		desired,
	)
}

// finalizeServerApply records the outcome of a statement run: fatal errors and
// partial applies save stale state and history; a clean run completes the
// server apply and updates the state store.
func finalizeServerApply(
	ctx context.Context,
	db *sql.DB,
	sp migrate.ServerPlan,
	env string,
	appliedStatements []string,
	failedSQL string,
	failedStatements []migrate.StatementFailure,
	applyErr error,
	store migrate.Storage,
	stateStore *migrate.StateStore,
	desired *reconcile.DesiredState,
) (applySummary, error) {
	if applyErr != nil {
		return applySummary{}, savePartialApply(
			ctx,
			db,
			sp,
			env,
			appliedStatements,
			failedSQL,
			failedStatements,
			applyErr,
			store,
			stateStore,
			true,
		)
	}

	if len(failedStatements) > 0 {
		applyErr = fmt.Errorf(
			"server %q: %d statement(s) failed",
			sp.Server,
			len(failedStatements),
		)
		if err := savePartialApply(
			ctx,
			db,
			sp,
			env,
			appliedStatements,
			failedSQL,
			failedStatements,
			applyErr,
			store,
			stateStore,
			false,
		); err != nil {
			return applySummary{}, err
		}
		fmt.Fprintf(
			os.Stderr,
			"# Server %q: partial apply recorded (%d statement(s) applied, %d failed)\n",
			sp.Server,
			len(appliedStatements),
			len(failedStatements),
		)
		return applySummary{
			AppliedStatements: len(appliedStatements),
			FailedStatements:  len(failedStatements),
			AffectedServers:   1,
		}, nil
	}

	_ = db.Close()

	if err := completeServerApply(ctx, sp, env, appliedStatements, store, stateStore, desired); err != nil {
		return applySummary{}, err
	}

	fmt.Fprintf(os.Stdout, "# Server %q: migration complete\n", sp.Server)
	return applySummary{
		AppliedStatements: len(appliedStatements),
	}, nil
}

// applyServerStatements executes each plan statement, recording applied and
// failed statements. It stops on the first pending interrupt or fatal error
// and returns the collected results plus the error to abort with.
func applyServerStatements(
	ctx context.Context,
	db *sql.DB,
	sp migrate.ServerPlan,
	roleProgMap map[string]string,
	progIgnoreMap map[string]*config.IgnoreErrorsConfig,
	serverIgnore *config.IgnoreErrorsConfig,
	warnOnError bool,
) ([]string, string, []migrate.StatementFailure, error) {
	var (
		applied   []string
		failedSQL string
		failures  []migrate.StatementFailure
		fatalErr  error
	)

	for _, stmt := range sp.Statements {
		// Check for interrupt before executing the next statement.
		if interrupted.Load() == 1 {
			fatalErr = fmt.Errorf("server %q: apply interrupted by signal", sp.Server)
			failedSQL = stmt.SQL
			fmt.Fprintf(os.Stderr, "  ! INTERRUPTED\n")
			break
		}

		if viper.GetBool("debug") {
			fmt.Fprintf(os.Stderr, "  Executing: %s\n", stmt.SQL)
		}

		if err := executeStatement(ctx, db, stmt.SQL); err != nil {
			errType := config.ClassifyError(err)
			progName := roleProgMap[stmt.Role]

			ignoredBy, ignored := ignoredErrorBy(
				serverIgnore,
				progIgnoreMap[progName],
				errType,
				sp.Server,
				progName,
			)
			if ignored {
				fmt.Fprintf(
					os.Stderr,
					"  ~ IGNORED [%s]: %s (%s ignores %q)\n",
					errType,
					stmt.SQL,
					ignoredBy,
					errType,
				)
				continue
			}

			failedSQL = stmt.SQL
			failures = append(failures, migrate.StatementFailure{
				SQL:       stmt.SQL,
				ErrorCode: string(errType),
				Error:     err.Error(),
			})
			if warnOnError {
				fmt.Fprintf(os.Stderr, "  ! WARNING [%s]: %s: %s\n", errType, stmt.SQL, err)
				continue
			}

			fatalErr = fmt.Errorf("executing on %q: %q: %w", sp.Server, stmt.SQL, err)
			fmt.Fprintf(os.Stderr, "  ! ERROR [%s]: %s\n", errType, stmt.SQL)
			break
		}

		fmt.Fprintf(os.Stdout, "  + %s\n", stmt.SQL)
		applied = append(applied, stmt.SQL)

		// Check for interrupt after executing a statement so the partial-save
		// path is entered even when the signal arrives during the last statement.
		if interrupted.Load() == 1 {
			fatalErr = fmt.Errorf("server %q: apply interrupted by signal", sp.Server)
			fmt.Fprintf(os.Stderr, "  ! INTERRUPTED\n")
			break
		}
	}

	return applied, failedSQL, failures, fatalErr
}

// ignoredErrorBy reports whether the error type is ignored by the server's or
// the statement's program ignore rules, and returns which one ignores it.
func ignoredErrorBy(
	serverIgnore *config.IgnoreErrorsConfig,
	progIgnore *config.IgnoreErrorsConfig,
	errType config.MySQLErrorCode,
	serverName, progName string,
) (string, bool) {
	if serverIgnore.ShouldIgnore(errType) {
		return fmt.Sprintf("server %q", serverName), true
	}
	if progIgnore.ShouldIgnore(errType) {
		return fmt.Sprintf("program %q", progName), true
	}
	return "", false
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
			ObjectType: g.ObjectType,
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
// closes the database connection, and optionally returns the original error.
// The stale marker changes the state checksum, forcing a re-plan before the next apply.
func savePartialApply(
	ctx context.Context,
	db *sql.DB,
	sp migrate.ServerPlan,
	env string,
	appliedStatements []string,
	failedSQL string,
	failedStatements []migrate.StatementFailure,
	applyErr error,
	store migrate.Storage,
	stateStore *migrate.StateStore,
	returnApplyErr bool,
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

	histErr := migrate.WriteHistory(ctx, store, migrate.HistoryEntry{
		Timestamp:   now,
		Environment: env,
		Server:      sp.Server,
		Statements:  appliedStatements,
		Checksum:    partialChecksum,
		Error:       applyErr.Error(),
		FailedSQL:   failedSQL,
		Failures:    failedStatements,
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
	if returnApplyErr {
		return applyErr
	}
	return nil
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
// and applies statements. Fatal errors stop iteration.
func applyAllServers(
	ctx context.Context,
	plan *migrate.PlanFile,
	srvs config.ServersFile,
	env string,
	store migrate.Storage,
	stateStore *migrate.StateStore,
	cfg *config.Config,
	progs config.ProgramsFile,
	warnOnError bool,
) (applySummary, error) {
	var summary applySummary
	for _, sp := range plan.Servers {
		if err := sp.Validate(); err != nil {
			return summary, fmt.Errorf("validating server plan for %q: %w", sp.Server, err)
		}

		result, err := applyServer(ctx, sp, srvs, env, store, stateStore, cfg, progs, warnOnError)
		if err != nil {
			return summary, err
		}
		summary.AppliedStatements += result.AppliedStatements
		summary.FailedStatements += result.FailedStatements
		summary.AffectedServers += result.AffectedServers
	}
	return summary, nil
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
