package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/na4ma4/mysql-role-reconciler/internal/config"
	"github.com/na4ma4/mysql-role-reconciler/internal/migrate"
	mysqlclient "github.com/na4ma4/mysql-role-reconciler/internal/mysql"
	"github.com/na4ma4/mysql-role-reconciler/internal/reconcile"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.yaml.in/yaml/v3"
)

var showCmd = &cobra.Command{
	Use:   "show -c config.yaml PLAN_FILE",
	Short: "Show plan details and drift",
	Long:  `Display the migration statements from a plan file. With --drift, also show config drift (desired vs last-applied) and server drift (current vs last-applied).`,
	Args:  cobra.ExactArgs(1),
	RunE:  runShow,
}

func init() {
	showCmd.Flags().Bool("drift", false, "Show drift diffs against last-applied state")
	showCmd.Flags().StringP("output", "o", "text", "Output format: text, markdown, json, yaml")

	rootCmd.AddCommand(showCmd)
}

func runShow(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true

	planPath := args[0]
	showDrift, _ := cmd.Flags().GetBool("drift")
	outputFormat, _ := cmd.Flags().GetString("output")

	var plan *migrate.PlanFile
	{
		var err error
		plan, err = migrate.ReadPlanFile(planPath)
		if err != nil {
			return fmt.Errorf("reading plan file: %w", err)
		}
	}

	var cfg *config.Config
	{
		var err error
		cfg, _, _, err = config.Load(viper.GetString("config"))
		if err != nil {
			return fmt.Errorf("loading config: %w", err)
		}
	}

	switch outputFormat {
	case "markdown", "md":
		return showMarkdown(plan, cfg.Roles)
	case "json":
		return showJSON(plan)
	case "yaml":
		return showYAML(plan)
	case "summary":
		return showSummary(plan)
	case "text", "":
		// continue below
	default:
		return fmt.Errorf("unknown output format %q (valid: text, markdown, json, yaml, summary)", outputFormat)
	}

	fmt.Fprintf(os.Stdout, "Plan generated at: %s\n", plan.GeneratedAt)
	fmt.Fprintf(os.Stdout, "Environment: %s\n", plan.Environment)
	fmt.Fprintln(os.Stdout)

	for _, sp := range plan.Servers {
		showServerPlan(sp)
	}

	if showDrift {
		ctx := context.Background()
		if err := displayDrift(ctx, plan, cfg); err != nil {
			return fmt.Errorf("displaying drift: %w", err)
		}
	}

	return nil
}

// showMarkdown outputs the plan in markdown format with collapsible sections per role,
// grouped by program within each server.
func showMarkdown(plan *migrate.PlanFile, roles []config.RoleConfig) error {
	// Collect summary counts across all servers
	totalCreates, totalGrants, totalRevokes, totalDrops := countStatements(plan)

	anchorList := make([]string, 0, len(plan.Servers))
	for _, sp := range plan.Servers {
		anchor := fmt.Sprintf("server-%s-%s", sp.Server, formatActionCountsAnchor(countServerStatements(sp)))
		anchorList = append(
			anchorList,
			fmt.Sprintf(" - [%s](#%s) <sub>%s</sub>", sp.Server, anchor, formatActionCounts(countServerStatements(sp))),
		)
	}

	fmt.Fprintln(os.Stdout, "# Summary")
	fmt.Fprintf(os.Stdout, "%s\n\n", formatActionCounts(totalCreates, totalGrants, totalRevokes, totalDrops))

	fmt.Fprintln(os.Stdout, "## Servers")
	fmt.Fprintf(os.Stdout, "%s\n\n", strings.Join(anchorList, "\n"))

	fmt.Fprintln(os.Stdout, "# Servers")

	for _, sp := range plan.Servers {
		srvCreates, srvGrants, srvRevokes, srvDrops := countServerStatements(sp)
		fmt.Fprintf(
			os.Stdout,
			"## Server %q (%s)\n\n[back to summary](#summary)\n\n",
			sp.Server,
			formatActionCounts(srvCreates, srvGrants, srvRevokes, srvDrops),
		)

		// Group statements by program (extracted from role name via template matching)
		progGroups := groupByProgram(sp, roles)
		for _, pg := range progGroups {
			fmt.Fprintf(
				os.Stdout,
				"### Program %q (%s)\n\n",
				pg.name,
				formatActionCounts(pg.creates, pg.grants, pg.revokes, pg.drops),
			)

			// Group by role within each program
			for _, rg := range pg.roles {
				fmt.Fprintln(os.Stdout, "<details>")
				fmt.Fprintf(
					os.Stdout,
					"<summary>%q (%s)</summary>\n",
					rg.roleName,
					formatActionCounts(rg.creates, rg.grants, rg.revokes, rg.drops),
				)
				fmt.Fprintln(os.Stdout)
				fmt.Fprintln(os.Stdout, "| Action | Database | Table | Permission |")
				fmt.Fprintln(os.Stdout, "| ------ | -------- | ----- | ---------- |")
				for _, s := range rg.stmts {
					db := formatDB(s.Database)
					tbl := formatTable(s.Table)
					if s.Type == reconcile.StatementCreateRole || s.Type == reconcile.StatementDropRole {
						db = ""
						tbl = ""
					}
					fmt.Fprintf(os.Stdout, "| %s | %s | %s | %s |\n", actionLabel(s), db, tbl, formatPermission(s))
				}
				fmt.Fprint(os.Stdout, "</details>\n\n")
			}
		}
	}

	return nil
}

func showJSON(plan *migrate.PlanFile) error {
	data, err := json.Marshal(plan)
	if err != nil {
		return fmt.Errorf("marshaling plan to JSON: %w", err)
	}
	fmt.Fprintln(os.Stdout, string(data))
	return nil
}

func showYAML(plan *migrate.PlanFile) error {
	data, err := yaml.Marshal(plan)
	if err != nil {
		return fmt.Errorf("marshaling plan to YAML: %w", err)
	}
	fmt.Fprint(os.Stdout, string(data))
	return nil
}

type roleGroup struct {
	roleName string
	stmts    []reconcile.MigrationStatement
	creates  int
	grants   int
	revokes  int
	drops    int
}

const programGroupGlobal = "global"

type programGroup struct {
	name    string
	roles   []roleGroup
	creates int
	grants  int
	revokes int
	drops   int
}

// tmplInfo holds the prefix and suffix around the {{name}} template placeholder.
type tmplInfo struct {
	prefix string
	suffix string
}

func groupByProgram(sp migrate.ServerPlan, roles []config.RoleConfig) []programGroup {
	tmpls := buildTemplateInfo(roles)
	roleMap, roleOrder := buildRoleGroups(sp.Statements)
	result := assignRolesToPrograms(roleOrder, roleMap, tmpls)
	sortProgramGroups(result)

	return result
}

func buildTemplateInfo(roles []config.RoleConfig) []tmplInfo {
	var tmpls []tmplInfo
	for _, r := range roles {
		if config.IsTemplate(r.Name) {
			prefix, suffix := splitTemplate(r.Name)
			tmpls = append(tmpls, tmplInfo{prefix, suffix})
		}
	}

	return tmpls
}

func buildRoleGroups(stmts []reconcile.MigrationStatement) (map[string]*roleGroup, []string) {
	roleMap := make(map[string]*roleGroup)
	roleOrder := make([]string, 0)

	for _, s := range stmts {
		rg, ok := roleMap[s.Role]
		if !ok {
			rg = &roleGroup{roleName: s.Role}
			roleMap[s.Role] = rg
			roleOrder = append(roleOrder, s.Role)
		}
		rg.stmts = append(rg.stmts, s)
		incrementRoleCounts(rg, s.Type)
	}

	return roleMap, roleOrder
}

func incrementRoleCounts(rg *roleGroup, t reconcile.StatementType) {
	switch t {
	case reconcile.StatementCreateRole:
		rg.creates++
	case reconcile.StatementGrant:
		rg.grants++
	case reconcile.StatementRevoke:
		rg.revokes++
	case reconcile.StatementDropRole:
		rg.drops++
	}
}

// extractProgramName returns the program name embedded in a templated role name,
// or an empty string if the role does not match any template.
func extractProgramName(roleName string, tmpls []tmplInfo) string {
	for _, t := range tmpls {
		if strings.HasPrefix(roleName, t.prefix) && strings.HasSuffix(roleName, t.suffix) {
			middle := roleName[len(t.prefix) : len(roleName)-len(t.suffix)]
			if middle != "" {
				return middle
			}
		}
	}

	return ""
}

func assignRolesToPrograms(
	roleOrder []string, roleMap map[string]*roleGroup, tmpls []tmplInfo,
) []programGroup {
	progMap := make(map[string]*programGroup)
	progOrder := make([]string, 0)
	globalGroup := &programGroup{name: programGroupGlobal}

	for _, roleName := range roleOrder {
		rg := roleMap[roleName]
		progName := extractProgramName(roleName, tmpls)

		pg := getOrCreateProgramGroup(progName, progMap, &progOrder, globalGroup)
		pg.roles = append(pg.roles, *rg)
		pg.creates += rg.creates
		pg.grants += rg.grants
		pg.revokes += rg.revokes
		pg.drops += rg.drops
	}

	result := make([]programGroup, 0, len(progOrder))
	for _, name := range progOrder {
		result = append(result, *progMap[name])
	}
	if len(globalGroup.roles) > 0 {
		result = append(result, *globalGroup)
	}

	return result
}

func getOrCreateProgramGroup(
	progName string, progMap map[string]*programGroup, progOrder *[]string,
	globalGroup *programGroup,
) *programGroup {
	if progName == "" {
		return globalGroup
	}

	pg, ok := progMap[progName]
	if !ok {
		pg = &programGroup{name: progName}
		progMap[progName] = pg
		*progOrder = append(*progOrder, progName)
	}

	return pg
}

func sortProgramGroups(result []programGroup) {
	sort.Slice(result, func(i, j int) bool {
		return programLess(result[i].name, result[j].name)
	})

	for i := range result {
		sortRoles(result[i].roles)
	}
}

func programLess(a, b string) bool {
	if a == programGroupGlobal {
		return false
	}
	if b == programGroupGlobal {
		return true
	}

	return a < b
}

func sortRoles(roles []roleGroup) {
	sort.Slice(roles, func(j, k int) bool {
		return roles[j].roleName < roles[k].roleName
	})
	for j := range roles {
		sortStatements(roles[j].stmts)
	}
}

func sortStatements(stmts []reconcile.MigrationStatement) {
	sort.Slice(stmts, func(a, b int) bool {
		return statementLess(stmts[a], stmts[b])
	})
}

func statementLess(a, b reconcile.MigrationStatement) bool {
	if a.Type != b.Type {
		return a.Type.CompareOrder() < b.Type.CompareOrder()
	}
	if a.Database != b.Database {
		return a.Database < b.Database
	}
	if a.Table != b.Table {
		return a.Table < b.Table
	}

	return formatPermission(a) < formatPermission(b)
}

func splitTemplate(name string) (string, string) {
	prefix, suffix, ok := strings.Cut(name, config.TemplateVar)
	if ok {
		return prefix, suffix
	}

	return name, ""
}

func countStatements(plan *migrate.PlanFile) (int, int, int, int) {
	creates, grants, revokes, drops := 0, 0, 0, 0

	for _, sp := range plan.Servers {
		c, g, r, d := countServerStatements(sp)
		creates += c
		grants += g
		revokes += r
		drops += d
	}

	return creates, grants, revokes, drops
}

// countServerStatements counts the number of each statement type in a server plan.
//
//	@returns	counts of creates, grants, revokes, drops.
func countServerStatements(sp migrate.ServerPlan) (int, int, int, int) {
	creates, grants, revokes, drops := 0, 0, 0, 0
	for _, s := range sp.Statements {
		switch s.Type {
		case reconcile.StatementCreateRole:
			creates++
		case reconcile.StatementGrant:
			grants++
		case reconcile.StatementRevoke:
			revokes++
		case reconcile.StatementDropRole:
			drops++
		}
	}

	return creates, grants, revokes, drops
}

func formatActionCounts(creates, grants, revokes, drops int) string {
	parts := make([]string, 0, 4) //nolint:mnd // Not a magic number, it's just the number of action types.
	if creates > 0 {
		parts = append(parts, fmt.Sprintf("%d create", creates))
	}
	if grants > 0 {
		parts = append(parts, fmt.Sprintf("%d grant", grants))
	}
	if revokes > 0 {
		parts = append(parts, fmt.Sprintf("%d revoke", revokes))
	}
	if drops > 0 {
		parts = append(parts, fmt.Sprintf("%d drop", drops))
	}
	if len(parts) == 0 {
		return "no changes"
	}
	return strings.Join(parts, ", ")
}

func formatActionCountsAnchor(creates, grants, revokes, drops int) string {
	parts := make([]string, 0, 4) //nolint:mnd // Not a magic number, it's just the number of action types.
	if creates > 0 {
		parts = append(parts, fmt.Sprintf("%d-create", creates))
	}
	if grants > 0 {
		parts = append(parts, fmt.Sprintf("%d-grant", grants))
	}
	if revokes > 0 {
		parts = append(parts, fmt.Sprintf("%d-revoke", revokes))
	}
	if drops > 0 {
		parts = append(parts, fmt.Sprintf("%d-drop", drops))
	}
	if len(parts) == 0 {
		return "no-changes"
	}
	return strings.Join(parts, "-")
}

func actionLabel(s reconcile.MigrationStatement) string {
	switch s.Type {
	case reconcile.StatementCreateRole:
		return "CREATE"
	case reconcile.StatementGrant:
		return "GRANT"
	case reconcile.StatementRevoke:
		return "REVOKE"
	case reconcile.StatementDropRole:
		return "DROP"
	default:
		return strings.ToUpper(s.Type.String())
	}
}

func formatDB(db string) string {
	if db == "" || db == "*" {
		return ""
	}
	return "`" + db + "`"
}

func formatTable(table string) string {
	if table == "" {
		return ""
	}
	if table == "*" {
		return "`*`"
	}
	return "`" + table + "`"
}

func formatPermission(s reconcile.MigrationStatement) string {
	//nolint:exhaustive // Only handle ROLE statements differently.
	switch s.Type {
	case reconcile.StatementCreateRole, reconcile.StatementDropRole:
		return "`ROLE`"
	default:
		// Extract permission list from SQL: "GRANT SELECT, INSERT ON ..."
		sql := s.SQL
		// Find content between GRANT/REVOKE and ON
		verb := ""
		if strings.HasPrefix(sql, "GRANT ") {
			verb = "GRANT "
		} else if strings.HasPrefix(sql, "REVOKE ") {
			verb = "REVOKE "
		}
		if verb == "" {
			return ""
		}
		sql = sql[len(verb):]
		before, _, ok := strings.Cut(sql, " ON ")
		if !ok {
			return sql
		}
		perms := before
		return "`" + perms + "`"
	}
}

func showServerPlan(sp migrate.ServerPlan) {
	fmt.Fprintf(os.Stdout, "Server: %s\n", sp.Server)
	fmt.Fprintf(os.Stdout, "Checksum: %s\n", sp.Checksum)

	if len(sp.Statements) == 0 {
		fmt.Fprintf(os.Stdout, "  No changes needed.\n\n")
		return
	}

	fmt.Fprintf(os.Stdout, "  Statements (%d):\n", len(sp.Statements))

	creates, grants, revokes, drops := groupStatements(sp.Statements)
	printStatementGroup("Create roles", creates)
	printStatementGroup("Grant permissions", grants)
	printStatementGroup("Revoke permissions", revokes)
	printStatementGroup("Drop roles", drops)

	fmt.Fprintln(os.Stdout)
}

func printStatementGroup(label string, stmts []reconcile.MigrationStatement) {
	if len(stmts) == 0 {
		return
	}

	fmt.Fprintf(os.Stdout, "    %s:\n", label)
	for _, s := range stmts {
		fmt.Fprintf(os.Stdout, "      %s\n", s.SQL)
	}
}

func showSummary(plan *migrate.PlanFile) error {
	type serverSummary struct {
		CreateRole int `json:"create_role"`
		Grant      int `json:"grant"`
		Revoke     int `json:"revoke"`
		DropRole   int `json:"drop_role"`
		Total      int `json:"_total"`
	}

	result := make(map[string]serverSummary, len(plan.Servers)+1)
	totals := serverSummary{}

	for _, sp := range plan.Servers {
		c, g, r, d := countServerStatements(sp)
		total := c + g + r + d
		result[sp.Server] = serverSummary{
			CreateRole: c,
			Grant:      g,
			Revoke:     r,
			DropRole:   d,
			Total:      total,
		}
		totals.CreateRole += c
		totals.Grant += g
		totals.Revoke += r
		totals.DropRole += d
		totals.Total += total
	}

	result["_total"] = totals

	data, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshaling summary: %w", err)
	}

	fmt.Fprintln(os.Stdout, string(data))
	return nil
}

func displayDrift(ctx context.Context, plan *migrate.PlanFile, cfg *config.Config) error {
	store, err := storageFromConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("creating storage: %w", err)
	}

	stateStore, err := migrate.LoadStateStore(ctx, store)
	if err != nil {
		return fmt.Errorf("loading state store: %w", err)
	}

	// Load servers for connecting during drift detection.
	_, srvs, _, err := config.Load(viper.GetString("config"))
	if err != nil {
		return fmt.Errorf("loading servers for drift: %w", err)
	}

	progs := config.FilterDisabledPrograms(
		config.FilterPrograms(cfg.Programs, nil),
	)

	fmt.Fprintln(os.Stdout, "=== Drift Analysis ===")

	for _, sp := range plan.Servers {
		lastState, exists := stateStore.Get(sp.Server)
		if !exists {
			fmt.Fprintf(os.Stdout, "Server %q: no previous apply on record\n\n", sp.Server)
			continue
		}

		if lastState.Stale {
			fmt.Fprintf(
				os.Stdout,
				"Server %q: STALE since %s — state is unreliable due to a previous partial apply (%s)\n",
				sp.Server,
				lastState.StaleAt,
				lastState.Error,
			)
		}

		srvCfg, srvOK := srvs[sp.Server]
		if !srvOK || !srvCfg.Enabled.Get() {
			fmt.Fprintf(
				os.Stdout,
				"Server %q: cannot compute config drift (server not found or disabled)\n\n",
				sp.Server,
			)
			continue
		}

		displayServerDrift(ctx, sp, srvCfg, cfg, progs, plan.Environment, lastState)
	}

	return nil
}

func displayServerDrift(
	ctx context.Context,
	sp migrate.ServerPlan,
	srvCfg config.ServerConfig,
	cfg *config.Config,
	progs config.ProgramsFile,
	env string,
	lastState migrate.ServerState,
) {
	lastEntries := migrate.GrantEntriesToReconcileEntries(lastState.Grants)

	// Config drift: compute full desired state from config + server connection.
	db, dbErr := mysqlclient.Connect(ctx, srvCfg)
	if dbErr != nil {
		fmt.Fprintf(os.Stdout, "Server %q: cannot connect for drift: %v\n\n", sp.Server, dbErr)
		return
	}

	desired := computeDesiredState(ctx, db, sp.Server, env, cfg, progs)
	_ = db.Close()

	desiredEntries := make([]reconcile.GrantEntry, len(desired.Grants))
	for i, g := range desired.Grants {
		desiredEntries[i] = reconcile.GrantEntry(g)
	}

	configDrift := reconcile.DiffGrants(desired.Roles, desiredEntries, lastState.Roles, lastEntries, false)
	if len(configDrift) == 0 {
		fmt.Fprintf(os.Stdout, "Server %q: no config drift since last apply\n", sp.Server)
	} else {
		fmt.Fprintf(
			os.Stdout,
			"Server %q: config drift (%d changes since last apply at %s):\n",
			sp.Server,
			len(configDrift),
			lastState.AppliedAt,
		)
		for _, s := range configDrift {
			fmt.Fprintf(os.Stdout, "  %s\n", s.SQL)
		}
	}

	// Server drift: read current actual state from the live server.
	actual, actualErr := readActualForDrift(ctx, srvCfg, cfg, progs, sp.Server, env)
	if actualErr != nil {
		fmt.Fprintf(os.Stdout, "Server %q: cannot compute server drift: %v\n\n", sp.Server, actualErr)
		return
	}

	actualEntries := make([]reconcile.GrantEntry, len(actual.Grants))
	for i, g := range actual.Grants {
		actualEntries[i] = reconcile.GrantEntry{
			Role:       g.Role,
			Database:   g.Database,
			Table:      g.Table,
			Privileges: g.Grants,
		}
	}

	serverDrift := reconcile.DiffGrants(lastState.Roles, lastEntries, actual.Roles, actualEntries, false)
	if len(serverDrift) == 0 {
		fmt.Fprintf(os.Stdout, "Server %q: no server drift\n", sp.Server)
	} else {
		fmt.Fprintf(
			os.Stdout,
			"Server %q: server drift (%d changes from last-applied state):\n",
			sp.Server,
			len(serverDrift),
		)
		for _, s := range serverDrift {
			fmt.Fprintf(os.Stdout, "  %s\n", s.SQL)
		}
	}

	fmt.Fprintln(os.Stdout)
}

// readActualForDrift connects to a server and reads the current actual state
// for drift detection purposes.
func readActualForDrift(
	ctx context.Context,
	srvCfg config.ServerConfig,
	cfg *config.Config,
	progs config.ProgramsFile,
	srvName, env string,
) (*mysqlclient.ActualState, error) {
	db, err := mysqlclient.Connect(ctx, srvCfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to server %q: %w", srvName, err)
	}
	defer db.Close()

	expandedRoles := config.ExpandRolesForServer(cfg.Roles, progs, srvName, env)
	roleNames := make([]string, len(expandedRoles))
	for i, r := range expandedRoles {
		roleNames[i] = r.Name
	}

	actual, err := mysqlclient.ReadActualState(ctx, db, srvCfg, roleNames)
	if err != nil {
		return nil, fmt.Errorf("reading actual state from %q: %w", srvName, err)
	}

	return actual, nil
}

func groupStatements(stmts []reconcile.MigrationStatement) (
	[]reconcile.MigrationStatement, []reconcile.MigrationStatement, []reconcile.MigrationStatement, []reconcile.MigrationStatement,
) {
	var creates, grants, revokes, drops []reconcile.MigrationStatement
	for _, s := range stmts {
		switch s.Type {
		case reconcile.StatementCreateRole:
			creates = append(creates, s)
		case reconcile.StatementGrant:
			grants = append(grants, s)
		case reconcile.StatementRevoke:
			revokes = append(revokes, s)
		case reconcile.StatementDropRole:
			drops = append(drops, s)
		}
	}

	return creates, grants, revokes, drops
}
