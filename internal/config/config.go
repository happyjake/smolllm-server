package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

const (
	DefaultBind       = "0.0.0.0:11435"
	DefaultLogLevel   = "info"
	EnvAccessKey      = "SMOLLLM_SERVER_ACCESS_KEY"
	EnvConfigPath     = "SMOLLLM_SERVER_CONFIG"
	DefaultConfigPath = "~/.config/smolllm-server/config.yaml"
	DefaultEnvFile    = "~/.env.smolllm"
)

type Config struct {
	Server  ServerConfig      `yaml:"server"`
	Aliases map[string]string `yaml:"aliases"`
}

type ServerConfig struct {
	Bind      string `yaml:"bind"`
	AccessKey string `yaml:"access_key"`
	EnvFile   string `yaml:"env_file"`
	LogLevel  string `yaml:"log_level"`
	// RequestTimeout bounds one upstream attempt in seconds, applied only when
	// the client did not send a `timeout` field. Without it a hung (not
	// failed) leg holds the chain for the library default (600s) before
	// fallback advances — a dead-but-hanging provider would pin every request
	// to leg 1. 0 keeps the library default.
	RequestTimeout float64 `yaml:"request_timeout"`
	// FirstOutputTimeout abandons a leg that produces no content or reasoning
	// delta for this many seconds and moves to the next one. Without it a leg
	// that is merely queued upstream holds the whole attempt budget, so the
	// chain never reaches a healthy model and the caller sees a timeout. Omit
	// the key to inherit the library default; 0 disables the check.
	FirstOutputTimeout *float64 `yaml:"first_output_timeout"`
	// ClientHeaderTimeout is how long the callers wait for response headers,
	// used only to validate the first-output budgets above. Defaults to 300s,
	// the pi client's documented header timeout.
	ClientHeaderTimeout float64 `yaml:"client_header_timeout"`
}

// Load reads YAML from path, applies env overrides and defaults, and validates.
// Use ResolvePath to figure out which path to pass.
func Load(path string) (*Config, error) {
	expanded, err := expandHome(path)
	if err != nil {
		return nil, fmt.Errorf("expand config path: %w", err)
	}
	data, err := os.ReadFile(expanded)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", expanded, err)
	}
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse yaml %s: %w", expanded, err)
	}

	cfg.applyDefaults()
	cfg.applyEnvOverrides()

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Bind == "" {
		c.Server.Bind = DefaultBind
	}
	if c.Server.LogLevel == "" {
		c.Server.LogLevel = DefaultLogLevel
	}
	if c.Server.EnvFile == "" {
		c.Server.EnvFile = DefaultEnvFile
	}
	if c.Aliases == nil {
		c.Aliases = map[string]string{}
	}
}

func (c *Config) applyEnvOverrides() {
	if v := os.Getenv(EnvAccessKey); v != "" {
		c.Server.AccessKey = v
	}
}

func (c *Config) Validate() error {
	if _, _, err := net.SplitHostPort(c.Server.Bind); err != nil {
		return fmt.Errorf("invalid server.bind %q: %w", c.Server.Bind, err)
	}
	if strings.TrimSpace(c.Server.AccessKey) == "" {
		return errors.New("server.access_key must be set in config (or via " + EnvAccessKey + ")")
	}
	switch strings.ToLower(c.Server.LogLevel) {
	case "debug", "info", "warn", "warning", "error":
	default:
		return fmt.Errorf("invalid server.log_level %q (want debug|info|warn|error)", c.Server.LogLevel)
	}
	if c.Server.RequestTimeout < 0 {
		return fmt.Errorf("server.request_timeout must be >= 0 seconds (got %g)", c.Server.RequestTimeout)
	}
	if c.Server.FirstOutputTimeout != nil && *c.Server.FirstOutputTimeout < 0 {
		return fmt.Errorf("server.first_output_timeout must be >= 0 seconds (got %g)", *c.Server.FirstOutputTimeout)
	}
	if err := validateFirstOutputBudget(c); err != nil {
		return err
	}
	for name, value := range c.Aliases {
		if strings.TrimSpace(name) == "" {
			return errors.New("alias name must not be empty")
		}
		if strings.ContainsAny(name, " \t/,") {
			return fmt.Errorf("alias name %q must not contain space, tab, slash, or comma", name)
		}
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("alias %q has empty value", name)
		}
		for _, part := range strings.Split(value, ",") {
			if strings.TrimSpace(part) == "" {
				return fmt.Errorf("alias %q has empty entry in chain", name)
			}
		}
	}
	return nil
}

// defaultClientHeaderTimeout matches the 300s headers timeout the bundled pi
// client applies to every request (DEFAULT_HTTP_IDLE_TIMEOUT_MS). It is a
// client-side fact this server cannot read, so it is configuration: whenever
// the two drift, a chain that exhausts its first-output budgets hangs until the
// client gives up and reports a timed-out request with no handshake at all.
const defaultClientHeaderTimeout = 300.0

// validateFirstOutputBudget keeps the worst case of a first-output-only chain
// inside the client's header timeout. Without this check the guard is easy to
// defeat: lengthen one budget, or add one more slow model to an alias, and the
// chain again spends longer than the caller will wait - which is exactly the
// incident the guard exists to prevent. Summing only first-output budgets (not
// the full attempt timeouts) is deliberate: once a leg emits output it has
// proven healthy, and the remaining generation time is bounded by the client's
// body timeout rather than by failover arithmetic.
func validateFirstOutputBudget(c *Config) error {
	if c.Server.FirstOutputTimeout == nil || *c.Server.FirstOutputTimeout == 0 {
		return nil
	}
	budget := *c.Server.FirstOutputTimeout
	limit := c.Server.ClientHeaderTimeout
	if limit <= 0 {
		limit = defaultClientHeaderTimeout
	}
	for name, value := range c.Aliases {
		legs := len(strings.Split(value, ","))
		worst := budget * float64(legs)
		if worst <= limit {
			continue
		}
		return fmt.Errorf(
			"alias %q has %d models; at first_output_timeout=%gs the worst case is %gs, exceeding the client header timeout of %gs. "+
				"Lower first_output_timeout (below %gs still fits %d models), shorten the chain, or raise server.client_header_timeout if the clients really wait longer",
			name, legs, budget, worst, limit, limit/float64(legs), legs)
	}
	return nil
}

// EnvFilePath returns the expanded env-file path.
func (c *Config) EnvFilePath() (string, error) {
	if c.Server.EnvFile == "" {
		return "", nil
	}
	return expandHome(c.Server.EnvFile)
}

// LoadEnvFile loads environment variables from the configured env file.
// Existing env vars are NOT overwritten — launchd or shell exports win.
// A missing file is not an error (returns nil).
func (c *Config) LoadEnvFile() error {
	return c.loadEnvFile(false)
}

// ReloadEnvFile re-sources the env file with overwrite enabled, so rotated
// provider keys actually take effect on SIGHUP. Use only on reload, not
// initial boot (initial boot must let launchd/shell exports win).
func (c *Config) ReloadEnvFile() error {
	return c.loadEnvFile(true)
}

func (c *Config) loadEnvFile(overload bool) error {
	path, err := c.EnvFilePath()
	if err != nil {
		return err
	}
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("stat env file %s: %w", path, err)
	}
	loader := godotenv.Load
	if overload {
		loader = godotenv.Overload
	}
	if err := loader(path); err != nil {
		return fmt.Errorf("load env file %s: %w", path, err)
	}
	return nil
}

// ResolveModel returns the smolllm model string for an alias, or the input
// itself if no alias matches.
func (c *Config) ResolveModel(name string) string {
	if v, ok := c.Aliases[name]; ok {
		return v
	}
	return name
}

// ResolvePath picks the config file path: explicit > env > default.
// Returns the expanded absolute path.
func ResolvePath(explicit string) (string, error) {
	candidate := explicit
	if candidate == "" {
		candidate = os.Getenv(EnvConfigPath)
	}
	if candidate == "" {
		candidate = DefaultConfigPath
	}
	expanded, err := expandHome(candidate)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", err
	}
	return abs, nil
}

// Store holds the live config behind an atomic pointer. Readers (handlers,
// auth middleware) call Get() per request; SIGHUP calls Reload() to swap in
// a freshly-parsed Config without dropping connections.
//
// Bind is captured at listen time and CANNOT be hot-changed; Reload logs
// such drift back to the caller via the returned diff but keeps serving on
// the original address. Everything else (aliases, access_key, log_level,
// env_file path) is read fresh on every request.
type Store struct {
	path string
	cur  atomic.Pointer[Config]
}

// NewStore wraps an already-loaded Config. The path is remembered so Reload
// can re-read the same file.
func NewStore(path string, initial *Config) *Store {
	s := &Store{path: path}
	s.cur.Store(initial)
	return s
}

// Get returns the current config snapshot. Cheap; safe for hot paths.
func (s *Store) Get() *Config { return s.cur.Load() }

// Path returns the config file path used by Reload.
func (s *Store) Path() string { return s.path }

// Reload re-reads the config file, validates, and atomically swaps in the
// new snapshot. Returns (new, old, error). On error the current snapshot is
// retained and the caller can keep serving with stale config.
func (s *Store) Reload() (newCfg, oldCfg *Config, err error) {
	loaded, err := Load(s.path)
	if err != nil {
		return nil, s.cur.Load(), err
	}
	old := s.cur.Swap(loaded)
	return loaded, old, nil
}

func expandHome(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if p == "~" {
			return home, nil
		}
		return filepath.Join(home, p[2:]), nil
	}
	return p, nil
}
