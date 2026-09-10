package main

import (
	"fmt"
	"os"
	"sort"

	"github.com/na4ma4/mysql-role-reconciler/internal/migrate"
	"github.com/na4ma4/mysql-role-reconciler/internal/reconcile"
	"github.com/spf13/cobra"
)

var sortPlanCmd = &cobra.Command{
	Use:   "sort-plan PLAN_FILE",
	Short: "Re-sort an existing plan file",
	Long: `Read a plan file, sort its servers, statements, and grants into deterministic
order, and rewrite it in place. The semantic content (SQL, checksum) is unchanged;
only the ordering is normalized so that output is consistent across runs.`,
	Args: cobra.ExactArgs(1),
	RunE: runSortPlan,
}

func init() {
	rootCmd.AddCommand(sortPlanCmd)
}

func runSortPlan(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true

	planPath := args[0]
	plan, err := migrate.ReadPlanFile(planPath)
	if err != nil {
		return fmt.Errorf("reading plan file: %w", err)
	}

	sortPlanFile(plan)

	// Convert back to Plan format and write
	plans := reconcilePlans(plan)

	if err = migrate.WritePlanFile(planPath, plan.Environment, plans); err != nil {
		return fmt.Errorf("writing plan file: %w", err)
	}

	fmt.Fprintf(os.Stdout, "Plan file %s re-sorted\n", planPath)
	return nil
}

func sortPlanFile(plan *migrate.PlanFile) {
	sort.Slice(plan.Servers, func(i, j int) bool {
		return plan.Servers[i].Server < plan.Servers[j].Server
	})

	for i := range plan.Servers {
		sortPlanServer(&plan.Servers[i])
	}
}

func sortPlanServer(plan *migrate.ServerPlan) {
	sort.Slice(plan.Statements, func(i, j int) bool {
		return sortPlanStatementLess(plan.Statements[i], plan.Statements[j])
	})
	sort.Strings(plan.Roles)
	sort.Slice(plan.Grants, func(i, j int) bool {
		return sortPlanGrantLess(plan.Grants[i], plan.Grants[j])
	})
}

func sortPlanStatementLess(a, b reconcile.MigrationStatement) bool {
	if a.Type.CompareOrder() != b.Type.CompareOrder() {
		return a.Type.CompareOrder() < b.Type.CompareOrder()
	}
	if a.Role != b.Role {
		return a.Role < b.Role
	}
	if a.Database != b.Database {
		return a.Database < b.Database
	}
	if a.ObjectType != b.ObjectType {
		return a.ObjectType < b.ObjectType
	}
	return a.Table < b.Table
}

func sortPlanGrantLess(a, b migrate.GrantEntry) bool {
	if a.Role != b.Role {
		return a.Role < b.Role
	}
	if a.Database != b.Database {
		return a.Database < b.Database
	}
	if a.ObjectType != b.ObjectType {
		return a.ObjectType < b.ObjectType
	}
	return a.Table < b.Table
}

func reconcilePlans(plan *migrate.PlanFile) []*reconcile.Plan {
	plans := make([]*reconcile.Plan, len(plan.Servers))
	for i, sp := range plan.Servers {
		plans[i] = &reconcile.Plan{
			Server:        sp.Server,
			Statements:    sp.Statements,
			Checksum:      sp.Checksum,
			Roles:         sp.Roles,
			Grants:        grantEntriesToDesired(sp.Grants),
			StateChecksum: sp.StateChecksum,
		}
	}
	return plans
}

func grantEntriesToDesired(entries []migrate.GrantEntry) []reconcile.DesiredGrant {
	grants := make([]reconcile.DesiredGrant, len(entries))
	for i, e := range entries {
		grants[i] = reconcile.DesiredGrant{
			Role:       e.Role,
			Database:   e.Database,
			Table:      e.Table,
			ObjectType: e.ObjectType,
			Privileges: e.Privileges,
		}
	}
	return grants
}
