// Package config loads and validates .doneguard.yml.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mkarvan/doneguard/internal/glob"
)

// FileNames are the config file names DoneGuard looks for, in order.
var FileNames = []string{".doneguard.yml", ".doneguard.yaml"}

const (
	// DefaultMaxRetries is used when max_retries is not set.
	DefaultMaxRetries = 3
	// HardMaxBlocks keeps DoneGuard under Claude Code's 8-consecutive-block cap.
	HardMaxBlocks = 7
	// DefaultOutputLimit is the default number of output characters in a reason.
	DefaultOutputLimit = 4000
	// DefaultTimeout is used for verify commands without a timeout.
	DefaultTimeout = 120 * time.Second
	// MaxTimeout bounds a single verify command's timeout.
	MaxTimeout = 24 * time.Hour
	// DataDirName is the per-project runtime directory (gitignored).
	DataDirName = ".doneguard"
)

// Command is one verify command.
type Command struct {
	Command string
	Timeout time.Duration
}

// Config is a parsed .doneguard.yml.
type Config struct {
	Path        string // absolute path of the config file
	Dir         string // directory containing the config file (project root)
	Verify      []Command
	Protected   []string
	MaxRetries  int
	VerifyCwd   string // absolute
	OutputLimit int
}

// EffectiveMaxRetries is max_retries clamped to stay under the platform block cap.
func (c *Config) EffectiveMaxRetries() int {
	if c.MaxRetries > HardMaxBlocks {
		return HardMaxBlocks
	}
	if c.MaxRetries < 0 {
		return 0
	}
	return c.MaxRetries
}

// DataDir is <project>/.doneguard.
func (c *Config) DataDir() string { return filepath.Join(c.Dir, DataDirName) }

// LogPath is <project>/.doneguard/sessions.jsonl.
func (c *Config) LogPath() string { return filepath.Join(c.DataDir(), "sessions.jsonl") }

// TotalTimeout is the sum of all verify command timeouts.
func (c *Config) TotalTimeout() time.Duration {
	var d time.Duration
	for _, v := range c.Verify {
		d += v.Timeout
	}
	return d
}

// Find walks up from dir looking for a config file. It returns "" if none.
func Find(dir string) string {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for {
		for _, name := range FileNames {
			p := filepath.Join(dir, name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// Load reads and parses a config file.
func Load(path string) (*Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(data, filepath.Dir(abs))
	if err != nil {
		return nil, err
	}
	cfg.Path = abs
	return cfg, nil
}

// Parse parses config data; dir is the project directory used to resolve verify_cwd.
func Parse(data []byte, dir string) (*Config, error) {
	root, err := parseYAML(string(data))
	if err != nil {
		return nil, err
	}
	cfg := &Config{
		Dir:         dir,
		MaxRetries:  DefaultMaxRetries,
		OutputLimit: DefaultOutputLimit,
		VerifyCwd:   dir,
	}
	if root.kind == nullNode {
		return cfg, nil
	}
	if root.kind != mapNode {
		return nil, errors.New("top level must be a mapping (key: value)")
	}
	for i, key := range root.keys {
		val := root.vals[i]
		switch key {
		case "verify":
			cmds, err := parseVerify(val)
			if err != nil {
				return nil, err
			}
			cfg.Verify = cmds
		case "protected":
			globs, err := stringList(val, "protected")
			if err != nil {
				return nil, err
			}
			if _, err := glob.Compile(globs); err != nil {
				return nil, errf(val.line, "%v", err)
			}
			cfg.Protected = globs
		case "max_retries":
			n, err := intValue(val, "max_retries")
			if err != nil {
				return nil, err
			}
			if n < 0 {
				return nil, errf(val.line, "max_retries must be >= 0")
			}
			cfg.MaxRetries = n
		case "output_limit":
			n, err := intValue(val, "output_limit")
			if err != nil {
				return nil, err
			}
			if n < 200 {
				return nil, errf(val.line, "output_limit must be >= 200")
			}
			cfg.OutputLimit = n
		case "verify_cwd":
			if val.kind != scalarNode {
				return nil, errf(val.line, "verify_cwd must be a string")
			}
			p := val.value
			if !filepath.IsAbs(p) {
				p = filepath.Join(dir, p)
			}
			cfg.VerifyCwd = filepath.Clean(p)
		default:
			return nil, errf(val.line, "unknown key %q (allowed: verify, protected, max_retries, verify_cwd, output_limit)", key)
		}
	}
	return cfg, nil
}

func parseVerify(n *node) ([]Command, error) {
	switch n.kind {
	case nullNode:
		return nil, nil
	case seqNode:
	default:
		return nil, errf(n.line, "verify must be a list of commands")
	}
	var out []Command
	for _, item := range n.items {
		cmd := Command{Timeout: DefaultTimeout}
		switch item.kind {
		case scalarNode:
			cmd.Command = item.value
		case mapNode:
			haveCmd := false
			for i, k := range item.keys {
				v := item.vals[i]
				switch k {
				case "command", "run":
					if v.kind != scalarNode {
						return nil, errf(v.line, "verify command must be a string")
					}
					if haveCmd {
						return nil, errf(v.line, "verify entry has more than one command/run key")
					}
					haveCmd = true
					cmd.Command = v.value
				case "timeout":
					d, err := parseDuration(v)
					if err != nil {
						return nil, err
					}
					cmd.Timeout = d
				default:
					return nil, errf(v.line, "unknown verify key %q (allowed: command, timeout)", k)
				}
			}
		default:
			return nil, errf(item.line, "verify entries must be a command string or {command, timeout}")
		}
		cmd.Command = strings.TrimSpace(cmd.Command)
		if cmd.Command == "" {
			return nil, errf(item.line, "verify entry has an empty command")
		}
		out = append(out, cmd)
	}
	return out, nil
}

func parseDuration(n *node) (time.Duration, error) {
	if n.kind != scalarNode {
		return 0, errf(n.line, "timeout must be a duration like 120s or 5m")
	}
	s := strings.TrimSpace(n.value)
	if secs, err := strconv.Atoi(s); err == nil {
		if secs <= 0 {
			return 0, errf(n.line, "timeout must be positive")
		}
		if secs > int(MaxTimeout/time.Second) {
			return 0, errf(n.line, "timeout must be at most %s", MaxTimeout)
		}
		return time.Duration(secs) * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, errf(n.line, "invalid timeout %q (use e.g. 120s, 5m)", s)
	}
	if d > MaxTimeout {
		return 0, errf(n.line, "timeout must be at most %s", MaxTimeout)
	}
	return d, nil
}

func stringList(n *node, name string) ([]string, error) {
	switch n.kind {
	case nullNode:
		return nil, nil
	case scalarNode:
		return []string{n.value}, nil
	case seqNode:
	default:
		return nil, errf(n.line, "%s must be a list of strings", name)
	}
	var out []string
	for _, item := range n.items {
		if item.kind != scalarNode {
			return nil, errf(item.line, "%s entries must be strings", name)
		}
		out = append(out, item.value)
	}
	return out, nil
}

func intValue(n *node, name string) (int, error) {
	if n.kind != scalarNode {
		return 0, errf(n.line, "%s must be an integer", name)
	}
	v, err := strconv.Atoi(strings.TrimSpace(n.value))
	if err != nil {
		return 0, errf(n.line, "%s must be an integer, got %q", name, n.value)
	}
	return v, nil
}

// Quote returns s as a YAML scalar, quoting when needed.
func Quote(s string) string {
	if s == "" {
		return `""`
	}
	needs := strings.ContainsAny(s, "#:\"'{}[],&*!|>%@`\t\\") ||
		strings.HasPrefix(s, "-") || strings.HasPrefix(s, "?") ||
		strings.TrimSpace(s) != s
	switch strings.ToLower(s) {
	case "null", "~", "true", "false", "yes", "no":
		needs = true
	}
	if !needs {
		return s
	}
	return strconv.Quote(s)
}

// Describe renders a short human description of the config, for CLI output.
func (c *Config) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "config: %s\n", c.Path)
	if len(c.Verify) == 0 {
		b.WriteString("verify: (none configured)\n")
	}
	for _, v := range c.Verify {
		fmt.Fprintf(&b, "verify: %s (timeout %s)\n", v.Command, v.Timeout)
	}
	fmt.Fprintf(&b, "protected: %s\n", strings.Join(c.Protected, ", "))
	fmt.Fprintf(&b, "max_retries: %d\n", c.MaxRetries)
	return b.String()
}
