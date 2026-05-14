package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/na4ma4/go-yamladv"
	"go.yaml.in/yaml/v3"
)

// Config represents the top-level configuration loaded from config.yaml.
type Config struct {
	ProgramsFile   string              `yaml:"programs_file"`
	ServersFile    string              `yaml:"servers_file"`
	Servers        ServersFile         `yaml:"servers"`
	Programs       ProgramsFile        `yaml:"programs"`
	PermissionSets map[string][]string `yaml:"permission_sets"`
	Roles          []RoleConfig        `yaml:"roles"`
	State          StateConfig         `yaml:"state"`
}

// StateConfig configures where state and history are persisted.
type StateConfig struct {
	Storage string   `yaml:"storage"` // "local" (default) or "s3"
	Dir     string   `yaml:"dir"`     // base directory for local storage
	S3      S3Config `yaml:"s3"`      // S3 configuration (used when storage=s3)
}

// S3Config holds S3-specific storage configuration.
type S3Config struct {
	Bucket string `yaml:"bucket"`
	Prefix string `yaml:"prefix"`
	Region string `yaml:"region"`
}

// TemplateVar is the placeholder used in role names for program name substitution.
const TemplateVar = "{{name}}"

// IsTemplate returns true if the role name contains the template placeholder.
func IsTemplate(name string) bool {
	return strings.Contains(name, TemplateVar)
}

// ExpandRole expands a role template by replacing {{name}} with the given program name.
// Returns the expanded RoleConfig. If the role name is not a template, returns a copy as-is.
func ExpandRole(role RoleConfig, programName string) RoleConfig {
	expanded := RoleConfig{
		Name:   strings.ReplaceAll(role.Name, TemplateVar, programName),
		Server: role.Server,
		AppDB:  role.AppDB,
		SupDB:  role.SupDB,
	}
	return expanded
}

// ExpandRolesForServer expands all role templates into concrete roles for each
// program on the given server and environment. Non-templated roles are included
// once (global across all programs). Templated roles are expanded per program.
func ExpandRolesForServer(roles []RoleConfig, progs ProgramsFile, serverName, env string) []RoleConfig {
	programDBs := BuildProgramDBMap(serverName, env, progs)
	var result []RoleConfig
	seen := make(map[string]struct{})

	for _, role := range roles {
		if IsTemplate(role.Name) {
			for progName := range programDBs {
				expanded := ExpandRole(role, progName)
				if _, ok := seen[expanded.Name]; ok {
					continue
				}
				seen[expanded.Name] = struct{}{}
				result = append(result, expanded)
			}
		} else {
			if _, ok := seen[role.Name]; ok {
				continue
			}
			seen[role.Name] = struct{}{}
			result = append(result, role)
		}
	}

	return result
}

// ProgramDBs maps program name → database lists for programs using a server.
type ProgramDBs struct {
	AppDBs []string
	SupDBs []string
}

// BuildProgramDBMap returns programs that use the given server for the given environment.
func BuildProgramDBMap(serverName, env string, progs ProgramsFile) map[string]ProgramDBs {
	result := make(map[string]ProgramDBs)
	for _, p := range progs {
		srvName, ok := p.Server[env]
		if !ok || srvName != serverName {
			continue
		}
		result[p.Name] = ProgramDBs{
			AppDBs: p.AppDB,
			SupDBs: p.SupDB,
		}
	}
	return result
}

// BuildRoleProgramMap builds a map from expanded role name to the program it belongs to.
// Only templated roles are included (non-templated roles have no program association).
// This enables O(1) lookup instead of O(templates * programs) reverse matching.
func BuildRoleProgramMap(roles []RoleConfig, programDBs map[string]ProgramDBs) map[string]string {
	result := make(map[string]string)
	for _, role := range roles {
		if !IsTemplate(role.Name) {
			continue
		}
		prefix, suffix := splitTemplate(role.Name)
		for progName := range programDBs {
			expandedName := prefix + progName + suffix
			result[expandedName] = progName
		}
	}
	return result
}

func splitTemplate(templateName string) (string, string) {
	prefix, suffix, ok := strings.Cut(templateName, TemplateVar)
	if !ok {
		return templateName, ""
	}
	return prefix, suffix
}

// ProgramConfig represents a program entry (from either config or programs file).
type ProgramConfig struct {
	Name         string             `yaml:"name"`
	Server       map[string]string  `yaml:"server"`
	AppDB        []string           `yaml:"app_db"`
	SupDB        []string           `yaml:"sup_db"`
	IgnoreErrors IgnoreErrorsConfig `yaml:"ignore_errors"`
	Enabled      ptrVal[bool]       `yaml:"enabled"`
}

// IgnoreErrorsConfig specifies which MySQL errors to ignore during apply for a program.
// It can be `true` (ignore all errors) or a list of named error types such as
// "table_not_found", "role_not_found", etc.
type IgnoreErrorsConfig struct {
	All    bool
	Errors []MySQLErrorCode
}

// ShouldIgnore returns true if the given error type should be ignored.
func (c *IgnoreErrorsConfig) ShouldIgnore(errType MySQLErrorCode) bool {
	if c == nil {
		return false
	}
	if c.All {
		return true
	}
	for _, e := range c.Errors {
		if e == errType || e == "all" {
			return true
		}
	}
	return false
}

// UnmarshalYAML allows ignore_errors to be true, a single string, or a list of strings.
func (c *IgnoreErrorsConfig) UnmarshalYAML(unmarshal func(any) error) error {
	// Try bool first (ignore_errors: true)
	var b bool
	if err := unmarshal(&b); err == nil {
		c.All = b
		return nil
	}

	{
		var list []MySQLErrorCode
		if err := unmarshal(&list); err == nil {
			c.Errors = list
			return nil
		}
	}

	// Try list of strings (ignore_errors: ["table_not_found", ...])
	var list []string
	if err := unmarshal(&list); err == nil {
		c.Errors = make([]MySQLErrorCode, len(list))
		for i, e := range list {
			c.Errors[i] = MySQLErrorCode(e)
		}
		return nil
	}

	// Try single string (ignore_errors: "table_not_found")
	var s string
	if err := unmarshal(&s); err == nil {
		c.Errors = []MySQLErrorCode{MySQLErrorCode(s)}
		return nil
	}

	return errors.New("ignore_errors must be true, a string, or a list of error names")
}

// MarshalYAML emits true when All is set and there are no specific names,
// otherwise emits the list. This round-trips cleanly with UnmarshalYAML.
func (c IgnoreErrorsConfig) MarshalYAML() (any, error) {
	if c.All && len(c.Errors) == 0 {
		return true, nil
	}
	if len(c.Errors) > 0 {
		return c.Errors, nil
	}
	//nolint:nilnil // (nil, nil) is intentional: signals "omit" to yaml.v3 marshaler
	return nil, nil
}

// RoleConfig represents a role definition with permission sets per scope.
type RoleConfig struct {
	Name   string              `yaml:"name"`
	Server map[string][]string `yaml:"server"`
	AppDB  map[string][]string `yaml:"app_db"`
	SupDB  map[string][]string `yaml:"sup_db"`
}

// ServerConfig represents connection details for a MySQL server.
type ServerConfig struct {
	Enabled         ptrVal[bool]          `yaml:"enabled"`
	Host            string                `yaml:"host"`
	Port            int                   `yaml:"port"`
	User            string                `yaml:"user"`
	Password        string                `yaml:"password"`
	AWSRegion       string                `yaml:"aws_region"`
	AWSID           string                `yaml:"aws_id"`
	IAMAuth         bool                  `yaml:"iam_auth"`
	SSL             SSLConfig             `yaml:"ssl"`
	OpenConnections ptrVal[int]           `yaml:"open_connections"`
	IdleConnections ptrVal[int]           `yaml:"idle_connections"`
	MaxConnLifetime ptrVal[time.Duration] `yaml:"max_conn_lifetime"`
}

//nolint:mnd // Magic numbers in default config are fine.
func (s *ServerConfig) setDefaults() {
	if !s.Enabled.IsSet() {
		s.Enabled.Set(true)
	}

	if s.Port == 0 {
		s.Port = 3306
	}

	if !s.OpenConnections.IsSet() {
		s.OpenConnections.Set(5)
	}

	if !s.IdleConnections.IsSet() {
		s.IdleConnections.Set(5)
	}

	if !s.MaxConnLifetime.IsSet() {
		s.MaxConnLifetime.Set(5 * time.Minute)
	}
}

// ID returns a unique identifier for the server, derived from the host name.
// For example, "myserver.prod.example.com" → "myserver".
//
//nolint:mnd // This is not a magic number, it's a specific parsing rule for server IDs.
func (s *ServerConfig) ID() string {
	return strings.SplitN(s.Host, ".", 2)[0]
}

// SSLConfig represents SSL/TLS connection parameters.
type SSLConfig struct {
	CA   string `yaml:"ca"`
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
}

// ServersFile represents the servers.yaml structure (map of server name → config).
type ServersFile map[string]ServerConfig

// ProgramsFile represents the programs.yaml structure.
// Supports two YAML formats:
//
//	List (original):
//	  - name: foobar
//	    server: { prod: hyperion }
//	    app_db: [zban_foobar]
//
//	Map (key is the program name):
//	  foobar:
//	    server: { prod: hyperion }
//	    app_db: [zban_foobar]
type ProgramsFile []ProgramConfig

// UnmarshalYAML accepts both the list and map formats for programs.
func (p *ProgramsFile) UnmarshalYAML(unmarshal func(any) error) error {
	// Try list format first: - name: foobar ...
	var list []ProgramConfig
	if err := unmarshal(&list); err == nil {
		*p = list
		return nil
	}

	// Try map format: foobar: { server: ..., app_db: ... }
	var m map[string]programMapEntry
	if err := unmarshal(&m); err == nil {
		// Deterministic order by sorting keys
		names := make([]string, 0, len(m))
		for name := range m {
			names = append(names, name)
		}
		sort.Strings(names)

		for _, name := range names {
			entry := m[name]
			pc := ProgramConfig{
				Name:         name,
				Server:       entry.Server,
				AppDB:        entry.AppDB,
				SupDB:        entry.SupDB,
				IgnoreErrors: entry.IgnoreErrors,
				Enabled:      entry.Enabled,
			}
			*p = append(*p, pc)
		}
		return nil
	}

	return errors.New("programs must be a list or a map")
}

// programMapEntry is the value type when programs are specified as a map.
// The key (program name) is injected into ProgramConfig.Name during conversion.
type programMapEntry struct {
	Server       map[string]string  `yaml:"server"`
	AppDB        []string           `yaml:"app_db"`
	SupDB        []string           `yaml:"sup_db"`
	IgnoreErrors IgnoreErrorsConfig `yaml:"ignore_errors"`
	Enabled      ptrVal[bool]       `yaml:"enabled"`
}

// Load reads and merges the full configuration from all files.
func Load(configPath string) (*Config, ServersFile, ProgramsFile, error) {
	var cfg *Config
	{
		var err error
		cfg, err = loadConfig(configPath)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("loading config: %w", err)
		}
	}

	baseDir := dirOf(configPath)

	var progs ProgramsFile
	if cfg.ProgramsFile != "" {
		var err error
		progs, err = loadPrograms(resolvePath(baseDir, cfg.ProgramsFile))
		if err != nil {
			return nil, nil, nil, fmt.Errorf("loading programs: %w", err)
		}
	}

	var srvs ServersFile
	if cfg.ServersFile != "" {
		var err error
		srvs, err = LoadServers(resolvePath(baseDir, cfg.ServersFile))
		if err != nil {
			return nil, nil, nil, fmt.Errorf("loading servers: %w", err)
		}
	}

	// Use inline servers/programs from resolved config if not loaded from separate files
	if len(srvs) == 0 && len(cfg.Servers) > 0 {
		srvs = cfg.Servers
		for name := range srvs {
			s := srvs[name]
			s.setDefaults()
			srvs[name] = s
		}
	}
	if len(progs) == 0 && len(cfg.Programs) > 0 {
		progs = cfg.Programs
	}

	progs = MergePrograms(progs, cfg.Programs)
	SetProgramDefaults(progs)

	if err := Validate(baseDir, cfg, srvs, progs); err != nil {
		return nil, nil, nil, fmt.Errorf("validation: %w", err)
	}

	return cfg, srvs, progs, nil
}

func loadConfig(path string) (*Config, error) {
	var root *yaml.Node
	{
		var err error
		root, err = loadResolvedNode(path)
		if err != nil {
			return nil, err
		}
	}

	var cfg Config
	if err := root.Decode(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// LoadResolvedYAML reads and resolves all include tags in the config file,
// then returns the fully-resolved YAML as bytes.
func LoadResolvedYAML(path string) ([]byte, error) {
	root, err := loadResolvedNode(path)
	if err != nil {
		return nil, err
	}

	out, err := yaml.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("marshaling resolved config: %w", err)
	}
	return out, nil
}

func loadResolvedNode(path string) (*yaml.Node, error) {
	var data []byte
	{
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, err
		}
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, err
	}

	baseDir := dirOf(path)
	if err := yamladv.Resolve(&root, baseDir); err != nil {
		return nil, fmt.Errorf("resolving includes: %w", err)
	}

	return &root, nil
}

func loadPrograms(path string) (ProgramsFile, error) {
	return LoadProgramsFile(path)
}

// LoadProgramsFile reads and parses a programs YAML file.
func LoadProgramsFile(path string) (ProgramsFile, error) {
	var data []byte
	{
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, err
		}
	}
	var progs ProgramsFile
	if err := yaml.Unmarshal(data, &progs); err != nil {
		return nil, err
	}
	return progs, nil
}

func LoadServers(path string) (ServersFile, error) {
	var data []byte
	{
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, err
		}
	}

	var srvs ServersFile
	if err := yaml.Unmarshal(data, &srvs); err != nil {
		return nil, err
	}

	for name := range srvs {
		srv := srvs[name]
		srv.setDefaults()
		srvs[name] = srv
	}

	return srvs, nil
}

// MergePrograms merges config programs into the programs file list.
// For matching program names, server/app_db/sup_db entries are appended (deduped).
// For new program names, the entry is appended.
// Returns the merged slice (may be a new allocation if appended).
func MergePrograms(fileProgs, cfgProgs ProgramsFile) ProgramsFile {
	for _, cp := range cfgProgs {
		found := false
		for i := range fileProgs {
			if fileProgs[i].Name == cp.Name {
				found = true
				fileProgs[i].Server = mergeMap(fileProgs[i].Server, cp.Server)
				fileProgs[i].AppDB = mergeSlice(fileProgs[i].AppDB, cp.AppDB)
				fileProgs[i].SupDB = mergeSlice(fileProgs[i].SupDB, cp.SupDB)
				// Config-level ignore_errors overrides file-level if set
				if cp.IgnoreErrors.All || len(cp.IgnoreErrors.Errors) > 0 {
					fileProgs[i].IgnoreErrors = cp.IgnoreErrors
				}
				// Config-level enabled overrides file-level if explicitly set
				if cp.Enabled.IsSet() {
					fileProgs[i].Enabled = cp.Enabled
				}
				break
			}
		}
		if !found {
			fileProgs = append(fileProgs, cp)
		}
	}
	return fileProgs
}

func mergeMap(existing, incoming map[string]string) map[string]string {
	if existing == nil {
		existing = make(map[string]string)
	}
	maps.Copy(existing, incoming)
	return existing
}

func mergeSlice(existing, incoming []string) []string {
	set := make(map[string]struct{})
	for _, s := range existing {
		set[s] = struct{}{}
	}
	for _, s := range incoming {
		if _, ok := set[s]; !ok {
			existing = append(existing, s)
			set[s] = struct{}{}
		}
	}
	return existing
}

// Validate checks config consistency. Program-server reference validation
// is deferred to ValidateProgramServers which should be called after
// program filtering (e.g., after --program flags are applied).
func Validate(baseDir string, cfg *Config, srvs ServersFile, progs ProgramsFile) error {
	if err := validateRoles(cfg); err != nil {
		return err
	}
	if err := validatePermissionSets(cfg); err != nil {
		return err
	}
	if err := validateProgramNames(progs); err != nil {
		return err
	}
	resolveSSLPaths(baseDir, srvs)
	return nil
}

// ValidateProgramServers checks that filtered programs reference only
// servers that exist in the servers file for the given environment.
// Call this after program filtering. If env is empty, all environments are checked.
func ValidateProgramServers(progs ProgramsFile, srvs ServersFile, env string) error {
	return validatePrograms(progs, srvs, env)
}

func validateRoles(cfg *Config) error {
	if len(cfg.Roles) == 0 {
		return errors.New("at least one role must be defined")
	}

	roleNames := make(map[string]struct{})
	for _, r := range cfg.Roles {
		if r.Name == "" {
			return errors.New("role name is required")
		}
		// Template role names are allowed to be "duplicates" at the template level;
		// they become unique after expansion. Only check non-template names.
		if !IsTemplate(r.Name) {
			if _, ok := roleNames[r.Name]; ok {
				return fmt.Errorf("duplicate role name: %s", r.Name)
			}
			roleNames[r.Name] = struct{}{}
		}
	}
	return nil
}

func validatePermissionSets(cfg *Config) error {
	for psName, perms := range cfg.PermissionSets {
		if len(perms) == 0 {
			return fmt.Errorf("permission_set %q has no permissions", psName)
		}
	}
	return nil
}

func validateProgramNames(progs ProgramsFile) error {
	for _, p := range progs {
		if p.Name == "" {
			return errors.New("program name is required")
		}
	}
	return nil
}

func validatePrograms(progs ProgramsFile, srvs ServersFile, env string) error {
	for _, p := range progs {
		for e, srvName := range p.Server {
			if env != "" && e != env {
				continue
			}
			if _, ok := srvs[srvName]; !ok {
				return fmt.Errorf("program %q references unknown server %q for environment %q", p.Name, srvName, e)
			}
		}
	}
	return nil
}

func resolveSSLPaths(baseDir string, srvs ServersFile) {
	for name, s := range srvs {
		if s.SSL.CA != "" {
			s.SSL.CA = resolvePath(baseDir, s.SSL.CA)
		}
		if s.SSL.Cert != "" {
			s.SSL.Cert = resolvePath(baseDir, s.SSL.Cert)
		}
		if s.SSL.Key != "" {
			s.SSL.Key = resolvePath(baseDir, s.SSL.Key)
		}
		srvs[name] = s
	}
}

func resolvePath(baseDir, path string) string {
	if strings.HasPrefix(path, "/") {
		return path
	}
	return baseDir + "/" + path
}

func dirOf(path string) string {
	idx := strings.LastIndex(path, "/")
	if idx < 0 {
		return "."
	}
	return path[:idx]
}

// FilterPrograms returns a ProgramsFile containing only programs whose names
// are in the allowlist. If allowlist is empty, returns the full list unchanged.
func FilterPrograms(progs ProgramsFile, allowlist []string) ProgramsFile {
	if len(allowlist) == 0 {
		return progs
	}

	allowed := make(map[string]struct{}, len(allowlist))
	for _, name := range allowlist {
		allowed[name] = struct{}{}
	}

	var filtered ProgramsFile
	for _, p := range progs {
		if _, ok := allowed[p.Name]; ok {
			filtered = append(filtered, p)
		}
	}
	return filtered
}

// SetProgramDefaults sets the Enabled default (true) for any program that
// doesn't explicitly set it. This mirrors ServerConfig.setDefaults.
func SetProgramDefaults(progs ProgramsFile) {
	for i := range progs {
		if !progs[i].Enabled.IsSet() {
			progs[i].Enabled.Set(true)
		}
	}
}

// FilterDisabledPrograms removes programs with enabled: false and returns
// the filtered list. Prints a message for each disabled program.
func FilterDisabledPrograms(progs ProgramsFile) ProgramsFile {
	var active ProgramsFile
	for _, p := range progs {
		if !p.Enabled.Get() {
			fmt.Fprintf(os.Stderr, "# Program %q: disabled, skipping\n", p.Name)
			continue
		}
		active = append(active, p)
	}
	return active
}
