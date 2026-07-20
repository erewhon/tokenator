// Package bench is `tokenator bench`: an A/B harness that runs the same
// task through multiple arms (harness + configuration variants), N trials
// each, in clean isolated sessions, then reports token/cost/duration/outcome
// per arm. See docs/bench.md.
package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Spec is one experiment definition, loaded from a JSON file. The resolved
// spec (defaults applied, prompt_file inlined) is snapshotted into
// bench_run.spec_json — resume always uses the snapshot, never the file.
type Spec struct {
	Name string `json:"name"`
	// Prompt is the task given to every arm. PromptFile inlines a file
	// instead (exactly one of the two must be set).
	Prompt     string `json:"prompt,omitempty"`
	PromptFile string `json:"prompt_file,omitempty"`
	// Workspace is a template directory copied fresh for every trial; the
	// agent runs with cwd inside the copy.
	Workspace string  `json:"workspace"`
	Checks    []Check `json:"checks,omitempty"`
	// Trials per arm (default 3).
	Trials int `json:"trials,omitempty"`
	// TimeoutSeconds per trial (default 900).
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// CheckTimeoutSeconds per check command (default 120).
	CheckTimeoutSeconds int `json:"check_timeout_seconds,omitempty"`
	// Env applies to every arm (arm env wins on conflict).
	Env  map[string]string `json:"env,omitempty"`
	Arms []Arm             `json:"arms"`
	// Regime is the billing regime recorded on per-trial sources
	// (default "subscription").
	Regime string `json:"regime,omitempty"`
}

// Check is a shell command run in the trial workspace after the agent
// finishes; exit 0 = pass.
type Check struct {
	Name string `json:"name"`
	Cmd  string `json:"cmd"`
}

// Arm is one experimental condition.
type Arm struct {
	Name    string `json:"name"`
	Harness string `json:"harness"` // claude_code | opencode
	// Model: CC alias/name (sonnet, opus, claude-...); OC provider/model.
	Model string            `json:"model,omitempty"`
	Env   map[string]string `json:"env,omitempty"`
	// ExtraArgs are appended verbatim to the harness command line — the
	// escape hatch for flags the typed options don't cover.
	ExtraArgs []string `json:"extra_args,omitempty"`
	Claude    *CCOpts  `json:"claude,omitempty"`
	OpenCode  *OCOpts  `json:"opencode,omitempty"`
}

// CCOpts maps to claude CLI flags (claude_code arms only).
//
// Note that a bench trial's CLAUDE_CONFIG_DIR is a fresh directory, so every
// claude_code arm already starts clean — no user settings, plugins, hooks,
// memory, or MCP servers. Options here ADD things back (plugin_dirs,
// mcp_configs, settings) or restrict further.
type CCOpts struct {
	// Bare passes --bare. CAUTION: bare mode never reads OAuth credentials
	// (auth is strictly ANTHROPIC_API_KEY or apiKeyHelper), so it fails on
	// subscription billing. A fresh CLAUDE_CONFIG_DIR is already clean —
	// only use bare with API-key arms.
	Bare bool `json:"bare,omitempty"`
	// PluginDirs each become a --plugin-dir flag.
	PluginDirs []string `json:"plugin_dirs,omitempty"`
	// MCPConfigs each become --mcp-config; when set, --strict-mcp-config
	// is also passed so ONLY these servers load.
	MCPConfigs []string `json:"mcp_configs,omitempty"`
	// Settings is a JSON object or file path for --settings.
	Settings string `json:"settings,omitempty"`
	// SettingSources for --setting-sources (e.g. "project").
	SettingSources string `json:"setting_sources,omitempty"`
	// PermissionMode for --permission-mode (default bypassPermissions —
	// trials run in throwaway workspace copies).
	PermissionMode string `json:"permission_mode,omitempty"`
	// Effort for --effort, when supported by the model.
	Effort string `json:"effort,omitempty"`
}

// OCOpts maps to opencode run flags (opencode arms only).
type OCOpts struct {
	// Pure passes --pure: no external plugins.
	Pure bool `json:"pure,omitempty"`
	// Agent for --agent.
	Agent string `json:"agent,omitempty"`
	// Variant for --variant (provider-specific reasoning effort).
	Variant string `json:"variant,omitempty"`
	// ConfigFile is exported as OPENCODE_CONFIG for the trial.
	ConfigFile string `json:"config_file,omitempty"`
}

const (
	HarnessClaudeCode = "claude_code"
	HarnessOpenCode   = "opencode"
)

// Load reads, resolves, and validates a spec file.
func Load(path string) (*Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Spec
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := s.resolve(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

// FromSnapshot rebuilds a resolved spec from bench_run.spec_json.
func FromSnapshot(specJSON string) (*Spec, error) {
	var s Spec
	if err := json.Unmarshal([]byte(specJSON), &s); err != nil {
		return nil, fmt.Errorf("spec snapshot: %w", err)
	}
	return &s, nil
}

// Snapshot serializes the resolved spec for bench_run.spec_json.
func (s *Spec) Snapshot() (string, error) {
	out, err := json.MarshalIndent(s, "", "  ")
	return string(out), err
}

// resolve applies defaults, inlines prompt_file (relative to the spec file's
// directory), and validates.
func (s *Spec) resolve(baseDir string) error {
	if s.Name == "" {
		return fmt.Errorf("name is required")
	}
	if strings.ContainsAny(s.Name, "/ \t") {
		return fmt.Errorf("name %q must be a single token (used in run keys and paths)", s.Name)
	}
	if (s.Prompt == "") == (s.PromptFile == "") {
		return fmt.Errorf("exactly one of prompt or prompt_file is required")
	}
	if s.PromptFile != "" {
		p := s.PromptFile
		if !filepath.IsAbs(p) {
			p = filepath.Join(baseDir, p)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("prompt_file: %w", err)
		}
		s.Prompt = string(data)
		s.PromptFile = ""
	}
	if s.Workspace == "" {
		return fmt.Errorf("workspace is required")
	}
	if !filepath.IsAbs(s.Workspace) {
		s.Workspace = filepath.Join(baseDir, s.Workspace)
	}
	if fi, err := os.Stat(s.Workspace); err != nil || !fi.IsDir() {
		return fmt.Errorf("workspace %s is not a directory", s.Workspace)
	}
	if s.Trials <= 0 {
		s.Trials = 3
	}
	if s.TimeoutSeconds <= 0 {
		s.TimeoutSeconds = 900
	}
	if s.CheckTimeoutSeconds <= 0 {
		s.CheckTimeoutSeconds = 120
	}
	if s.Regime == "" {
		s.Regime = "subscription"
	}
	if len(s.Arms) == 0 {
		return fmt.Errorf("at least one arm is required")
	}
	seen := map[string]bool{}
	for i := range s.Arms {
		a := &s.Arms[i]
		if a.Name == "" {
			return fmt.Errorf("arm %d: name is required", i)
		}
		if strings.ContainsAny(a.Name, "/ \t") {
			return fmt.Errorf("arm %q: name must be a single token", a.Name)
		}
		if seen[a.Name] {
			return fmt.Errorf("duplicate arm name %q", a.Name)
		}
		seen[a.Name] = true
		switch a.Harness {
		case HarnessClaudeCode:
			if a.OpenCode != nil {
				return fmt.Errorf("arm %q: opencode options on a claude_code arm", a.Name)
			}
		case HarnessOpenCode:
			if a.Claude != nil {
				return fmt.Errorf("arm %q: claude options on an opencode arm", a.Name)
			}
		default:
			return fmt.Errorf("arm %q: unknown harness %q (want %s or %s)",
				a.Name, a.Harness, HarnessClaudeCode, HarnessOpenCode)
		}
	}
	for i, c := range s.Checks {
		if c.Cmd == "" {
			return fmt.Errorf("check %d: cmd is required", i)
		}
		if c.Name == "" {
			s.Checks[i].Name = fmt.Sprintf("check%d", i+1)
		}
	}
	return nil
}
