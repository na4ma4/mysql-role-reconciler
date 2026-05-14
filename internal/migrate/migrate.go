package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/na4ma4/go-permbits"
	"github.com/na4ma4/mysql-role-reconciler/internal/reconcile"
	"go.yaml.in/yaml/v3"
)

const (
	// StateFileRelPath is the relative path of the state store file within storage.
	StateFileRelPath = ".mysql-reconciler-state.json"

	// HistoryPrefix is the relative path prefix for history entries within storage.
	HistoryPrefix = ".mysql-reconciler-history"

	// Version1 is the version string for the v1 history file format (YAML-like, sequence-number filenames).
	Version1 = "v1"

	// Version2 is the version string for the v2 history file format (proper YAML, UUIDv7 filenames).
	Version2 = "v2"
)

var (
	ErrInvalidPlanFile   = errors.New("invalid plan file")
	ErrMissingServerName = errors.New("missing server name in plan")
	ErrMissingChecksum   = errors.New("missing checksum in plan")
	ErrChecksumMismatch  = errors.New("checksum validation failed for plan")
	ErrStateChanged      = errors.New("state has changed since plan was generated")
)

// PlanFile represents the JSON structure of a plan file.
type PlanFile struct {
	GeneratedAt string       `json:"generated_at"`
	Environment string       `json:"environment"`
	Servers     []ServerPlan `json:"servers"`
}

// ServerPlan represents the migration plan for a single server.
type ServerPlan struct {
	Server        string                         `json:"server"`
	Statements    []reconcile.MigrationStatement `json:"statements"`
	Checksum      string                         `json:"checksum"`
	Roles         []string                       `json:"roles"`
	Grants        []GrantEntry                   `json:"grants"`
	StateChecksum string                         `json:"state_checksum,omitempty"`
}

func (e *ServerPlan) Validate() error {
	if e.Server == "" {
		return ErrMissingServerName
	}
	if e.Checksum == "" {
		return ErrMissingChecksum
	}
	if !e.ValidateChecksum() {
		return ErrChecksumMismatch
	}

	return nil
}

func (e *ServerPlan) ValidateChecksum() bool {
	computed := ComputeChecksum(e.Statements)
	return computed == e.Checksum
}

// WritePlanFile writes a plan to the specified file path as JSON.
func WritePlanFile(path string, env string, plans []*reconcile.Plan) error {
	pf := PlanFile{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Environment: env,
		Servers:     make([]ServerPlan, len(plans)),
	}

	for i, p := range plans {
		filteredRoles, filteredGrants := FilterChangedState(p.Roles, desiredGrantsToEntries(p.Grants), p.Statements)
		pf.Servers[i] = ServerPlan{
			Server:        p.Server,
			Statements:    p.Statements,
			Checksum:      p.Checksum,
			Roles:         filteredRoles,
			Grants:        filteredGrants,
			StateChecksum: p.StateChecksum,
		}
	}

	var data []byte
	{
		var err error
		data, err = json.Marshal(pf)
		if err != nil {
			return fmt.Errorf("marshaling plan: %w", err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), permbits.MustString("u=rwx,go=rx")); err != nil {
		return fmt.Errorf("creating plan directory: %w", err)
	}

	if err := os.WriteFile(path, data, permbits.MustString("u=rw")); err != nil {
		return fmt.Errorf("writing plan file: %w", err)
	}

	return nil
}

// ReadPlanFile reads a plan file from the specified path.
func ReadPlanFile(path string) (*PlanFile, error) {
	var data []byte
	{
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading plan file: %w", err)
		}
	}

	var pf PlanFile
	if err := json.Unmarshal(data, &pf); err != nil {
		return nil, fmt.Errorf("parsing plan file: %w", err)
	}

	return &pf, nil
}

// ComputeChecksum generates a SHA256 checksum for a list of statements.
func ComputeChecksum(stmts []reconcile.MigrationStatement) string {
	sqls := make([]string, 0, len(stmts))
	for _, s := range stmts {
		sqls = append(sqls, s.SQL)
	}
	return ComputeChecksumFromSQL(sqls)
}

// ComputeChecksumFromSQL generates a SHA256 checksum for a list of SQL strings.
func ComputeChecksumFromSQL(sqls []string) string {
	sorted := make([]string, len(sqls))
	copy(sorted, sqls)
	sort.Strings(sorted)

	h := sha256.Sum256([]byte(strings.Join(sorted, ";")))
	return hex.EncodeToString(h[:])
}

// HistoryEntry represents a single applied migration in history.
type HistoryEntry struct {
	Version     string   `json:"version"              yaml:"version"`
	Timestamp   string   `json:"timestamp"            yaml:"timestamp"`
	Environment string   `json:"environment"          yaml:"environment"`
	Server      string   `json:"server"               yaml:"server"`
	Statements  []string `json:"statements"           yaml:"statements"`
	Checksum    string   `json:"checksum"             yaml:"checksum"`
	Error       string   `json:"error,omitempty"      yaml:"error,omitempty"`
	FailedSQL   string   `json:"failed_sql,omitempty" yaml:"failed_sql,omitempty"`
}

func (e *HistoryEntry) ValidateChecksum() bool {
	stmts := make([]reconcile.MigrationStatement, 0, len(e.Statements))
	for _, s := range e.Statements {
		stmts = append(stmts, reconcile.MigrationStatement{SQL: s})
	}
	computed := ComputeChecksum(stmts)
	return computed == e.Checksum
}

// WriteHistory appends a history entry for an applied migration using the given storage.
// History files are named with a time-ordered UUIDv7, ensuring chronological sort order
// without requiring a scan of existing files.
func WriteHistory(ctx context.Context, store Storage, entry HistoryEntry) error {
	entry.Version = Version2

	var id uuid.UUID
	{
		var err error
		id, err = uuid.NewV7()
		if err != nil {
			return fmt.Errorf("generating history UUID: %w", err)
		}
	}

	relPath := HistoryPrefix + "/" + id.String() + ".yaml"

	var data []byte
	{
		var err error
		data, err = yaml.Marshal(entry)
		if err != nil {
			return fmt.Errorf("marshaling history entry: %w", err)
		}
	}

	if err := store.WriteFile(ctx, relPath, data); err != nil {
		return fmt.Errorf("writing history entry: %w", err)
	}

	return nil
}

// ReadHistory reads all history entries from the history storage.
func ReadHistory(ctx context.Context, store Storage) ([]HistoryEntry, error) {
	var names []string
	{
		var err error
		names, err = store.ListFiles(ctx, HistoryPrefix)
		if err != nil {
			return nil, fmt.Errorf("listing history: %w", err)
		}
	}

	sort.Strings(names)

	var result []HistoryEntry
	for _, name := range names {
		relPath := HistoryPrefix + "/" + name

		var data []byte
		{
			var err error
			data, err = store.ReadFile(ctx, relPath)
			if err != nil {
				continue
			}
		}

		var entry HistoryEntry
		if err := yaml.Unmarshal(data, &entry); err == nil {
			result = append(result, entry)
			continue
		}

		if err := json.Unmarshal(data, &entry); err == nil {
			result = append(result, entry)
			continue
		}

		result = append(result, parseSimpleHistoryEntry(string(data)))
	}

	return result, nil
}

func parseSimpleHistoryEntry(data string) HistoryEntry {
	entry := HistoryEntry{}
	for line := range strings.SplitSeq(data, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "version:"):
			entry.Version = unquote(strings.TrimPrefix(line, "version:"))
		case strings.HasPrefix(line, "timestamp:"):
			entry.Timestamp = unquote(strings.TrimPrefix(line, "timestamp:"))
		case strings.HasPrefix(line, "environment:"):
			entry.Environment = unquote(strings.TrimPrefix(line, "environment:"))
		case strings.HasPrefix(line, "server:"):
			entry.Server = unquote(strings.TrimPrefix(line, "server:"))
		case strings.HasPrefix(line, "checksum:"):
			entry.Checksum = unquote(strings.TrimPrefix(line, "checksum:"))
		case strings.HasPrefix(line, "- "):
			entry.Statements = append(entry.Statements, unquote(strings.TrimPrefix(line, "- ")))
		}
	}
	return entry
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "\"")
	return s
}

// FilterChangedState returns only the roles and grants that are affected by the
// given migration statements. A role is included if it has any statement. A grant
// is included if its role has a create_role statement (all grants for the new role)
// or if its (role, database, table) matches a grant/revoke statement target.
func FilterChangedState(
	allRoles []string,
	allGrants []GrantEntry,
	stmts []reconcile.MigrationStatement,
) ([]string, []GrantEntry) {
	// Roles that have any statement
	changedRoles := make(map[string]struct{})
	// Roles with create_role: all their grants are included
	createdRoles := make(map[string]struct{})
	// Specific targets with grant/revoke: (role, db, table) → true
	changedTargets := make(map[[3]string]struct{})

	for _, s := range stmts {
		changedRoles[s.Role] = struct{}{}
		if s.Type == reconcile.StatementCreateRole {
			createdRoles[s.Role] = struct{}{}
		}
		if s.Type == reconcile.StatementGrant || s.Type == reconcile.StatementRevoke {
			changedTargets[[3]string{s.Role, s.Database, s.Table}] = struct{}{}
		}
	}

	var filteredRoles []string
	for _, r := range allRoles {
		if _, ok := changedRoles[r]; ok {
			filteredRoles = append(filteredRoles, r)
		}
	}

	var filteredGrants []GrantEntry
	for _, g := range allGrants {
		// All grants for newly created roles
		if _, ok := createdRoles[g.Role]; ok {
			filteredGrants = append(filteredGrants, g)
			continue
		}
		// Grants matching a specific changed target
		key := [3]string{g.Role, g.Database, g.Table}
		if _, ok := changedTargets[key]; ok {
			filteredGrants = append(filteredGrants, g)
		}
	}

	return filteredRoles, filteredGrants
}

func desiredGrantsToEntries(grants []reconcile.DesiredGrant) []GrantEntry {
	entries := make([]GrantEntry, len(grants))
	for i, g := range grants {
		entries[i] = GrantEntry{
			Role:       g.Role,
			Database:   g.Database,
			Table:      g.Table,
			Privileges: g.Privileges,
		}
	}
	return entries
}

// GrantEntriesToReconcileEntries converts stored GrantEntry to reconcile.GrantEntry.
func GrantEntriesToReconcileEntries(entries []GrantEntry) []reconcile.GrantEntry {
	result := make([]reconcile.GrantEntry, len(entries))
	for i, e := range entries {
		result[i] = reconcile.GrantEntry{
			Role:       e.Role,
			Database:   e.Database,
			Table:      e.Table,
			Privileges: e.Privileges,
		}
	}
	return result
}

// StateStore tracks the last-applied desired state per server for drift detection.
type StateStore struct {
	storage Storage
	entries map[string]ServerState // server name → state
}

// GrantEntry represents a single grant in the stored state.
type GrantEntry struct {
	Role       string   `json:"role"`
	Database   string   `json:"database"`
	Table      string   `json:"table"`
	Privileges []string `json:"privileges"`
}

// ServerState represents the last-applied state for a server.
// It stores the full desired state at apply time, enabling three-way diffs:
//   - Desired vs Current (live server): migration SQL
//   - Desired vs Last-applied: config drift since last apply
//   - Current (live) vs Last-applied: server drift since last apply
type ServerState struct {
	AppliedAt   string       `json:"applied_at"`
	Environment string       `json:"environment"`
	Checksum    string       `json:"checksum"`
	Roles       []string     `json:"roles"`
	Grants      []GrantEntry `json:"grants"`
	Stale       bool         `json:"stale,omitempty"`
	StaleAt     string       `json:"stale_at,omitempty"`
	Error       string       `json:"error,omitempty"`
}

// LoadStateStore reads the state store from the given storage backend.
func LoadStateStore(ctx context.Context, store Storage) (*StateStore, error) {
	var data []byte
	{
		var err error
		data, err = store.ReadFile(ctx, StateFileRelPath)
		if err != nil {
			//nolint:nilerr // Not found is acceptable — start with empty state
			return &StateStore{storage: store, entries: make(map[string]ServerState)}, nil
		}
	}

	var entries map[string]ServerState
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parsing state store: %w", err)
	}

	return &StateStore{storage: store, entries: entries}, nil
}

// Get returns the state for a given server.
func (s *StateStore) Get(server string) (ServerState, bool) {
	state, ok := s.entries[server]
	return state, ok
}

// Update updates the state for a given server and writes to storage.
func (s *StateStore) Update(ctx context.Context, server string, state ServerState) error {
	s.entries[server] = state

	var data []byte
	{
		var err error
		data, err = json.Marshal(s.entries)
		if err != nil {
			return fmt.Errorf("marshaling state store: %w", err)
		}
	}

	if err := s.storage.WriteFile(ctx, StateFileRelPath, data); err != nil {
		return fmt.Errorf("writing state store: %w", err)
	}

	return nil
}

// ChecksumFor returns a deterministic checksum for the current state of a server.
// Returns empty string if no state exists for the server.
func (s *StateStore) ChecksumFor(server string) string {
	state, ok := s.entries[server]
	if !ok {
		return ""
	}
	return ComputeStateChecksum(state)
}

// ComputeStateChecksum produces a deterministic SHA256 fingerprint of a ServerState,
// so that any change to the state is detectable. Grants and privileges are sorted
// before hashing to ensure canonical output.
func ComputeStateChecksum(state ServerState) string {
	roles := make([]string, len(state.Roles))
	copy(roles, state.Roles)
	sort.Strings(roles)

	type canonicalGrant struct {
		Role       string   `json:"role"`
		Database   string   `json:"database"`
		Table      string   `json:"table"`
		Privileges []string `json:"privileges"`
	}

	grants := make([]canonicalGrant, len(state.Grants))
	for i, g := range state.Grants {
		privs := make([]string, len(g.Privileges))
		copy(privs, g.Privileges)
		sort.Strings(privs)
		grants[i] = canonicalGrant{
			Role:       g.Role,
			Database:   g.Database,
			Table:      g.Table,
			Privileges: privs,
		}
	}
	sort.Slice(grants, func(i, j int) bool {
		if grants[i].Role != grants[j].Role {
			return grants[i].Role < grants[j].Role
		}
		if grants[i].Database != grants[j].Database {
			return grants[i].Database < grants[j].Database
		}
		return grants[i].Table < grants[j].Table
	})

	canonical := struct {
		Environment string           `json:"environment"`
		Checksum    string           `json:"checksum"`
		Roles       []string         `json:"roles"`
		Grants      []canonicalGrant `json:"grants"`
		Stale       bool             `json:"stale"`
		StaleAt     string           `json:"stale_at"`
		Error       string           `json:"error"`
	}{
		Environment: state.Environment,
		Checksum:    state.Checksum,
		Roles:       roles,
		Grants:      grants,
		Stale:       state.Stale,
		StaleAt:     state.StaleAt,
		Error:       state.Error,
	}

	data, err := json.Marshal(canonical)
	if err != nil {
		return ""
	}

	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// MarkStale marks the state for a server as stale and writes to storage.
// If no prior state exists, a minimal stale entry is created. The Stale flag
// and StaleAt timestamp change the checksum, which will cause any plan generated
// against the previous state to fail validation on the next apply. StaleAt ensures
// that even identical errors at different times produce different checksums.
func (s *StateStore) MarkStale(ctx context.Context, server, env, errMsg string) error {
	state, exists := s.entries[server]
	if !exists {
		state = ServerState{
			Environment: env,
		}
	}
	state.Stale = true
	state.StaleAt = time.Now().UTC().Format(time.RFC3339)
	state.Error = errMsg
	return s.Update(ctx, server, state)
}
