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

	var plan *migrate.PlanFile
	{
		var err error
		plan, err = migrate.ReadPlanFile(planPath)
		if err != nil {
			return fmt.Errorf("reading plan file: %w", err)
		}
	}

	// Sort servers by name
	sort.Slice(plan.Servers, func(i, j int) bool {
		return plan.Servers[i].Server < plan.Servers[j].Server
	})

	for i := range plan.Servers {
		sp := &plan.Servers[i]

		// Sort statements by type order, then role, database, table
		sort.Slice(sp.Statements, func(a, b int) bool {
			sa, sb := sp.Statements[a], sp.Statements[b]
			if sa.Type.CompareOrder() != sb.Type.CompareOrder() {
				return sa.Type.CompareOrder() < sb.Type.CompareOrder()
			}
			if sa.Role != sb.Role {
				return sa.Role < sb.Role
			}
			if sa.Database != sb.Database {
				return sa.Database < sb.Database
			}
			return sa.Table < sb.Table
		})

		// Sort roles
		sort.Strings(sp.Roles)

		// Sort grants by role, database, table
		sort.Slice(sp.Grants, func(a, b int) bool {
			ga, gb := sp.Grants[a], sp.Grants[b]
			if ga.Role != gb.Role {
				return ga.Role < gb.Role
			}
			if ga.Database != gb.Database {
				return ga.Database < gb.Database
			}
			return ga.Table < gb.Table
		})
	}

	// Convert back to Plan format and write
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

	if err := migrate.WritePlanFile(planPath, plan.Environment, plans); err != nil {
		return fmt.Errorf("writing plan file: %w", err)
	}

	fmt.Fprintf(os.Stdout, "Plan file %s re-sorted\n", planPath)
	return nil
}

func grantEntriesToDesired(entries []migrate.GrantEntry) []reconcile.DesiredGrant {
	grants := make([]reconcile.DesiredGrant, len(entries))
	for i, e := range entries {
		grants[i] = reconcile.DesiredGrant{
			Role:       e.Role,
			Database:   e.Database,
			Table:      e.Table,
			Privileges: e.Privileges,
		}
	}
	return grants
}
