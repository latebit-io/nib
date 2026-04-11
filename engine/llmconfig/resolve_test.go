package llmconfig

import (
	"os"
	"path/filepath"
	"testing"
)

type resolveTestCase struct {
	name        string
	globalJSON  string
	projectJSON string
	env         map[string]string
	wantProfile string
	wantBaseURL string
	wantModel   string
	wantKeyEnv  string
	wantHas     bool
	wantCaching bool
}

var resolveTests = []resolveTestCase{
	{
		name:        "defaults only",
		wantProfile: "env",
		wantBaseURL: DefaultBaseURL,
		wantModel:   DefaultModel,
		wantKeyEnv:  DefaultKeyEnv,
		wantHas:     false,
	},
	{
		name:        "env vars only",
		env:         map[string]string{"LLM_API_KEY": "sk-test", "LLM_MODEL": "gpt-4o"},
		wantProfile: "env",
		wantBaseURL: DefaultBaseURL,
		wantModel:   "gpt-4o",
		wantKeyEnv:  DefaultKeyEnv,
		wantHas:     true,
	},
	{
		name: "global file sets profile",
		globalJSON: `{
				"profiles": {
					"openrouter": {
						"base_url": "https://openrouter.ai/api/v1",
						"model": "anthropic/claude-sonnet-4",
						"api_key_env": "OR_KEY"
					}
				},
				"active": "openrouter"
			}`,
		env:         map[string]string{"OR_KEY": "or-secret"},
		wantProfile: "openrouter",
		wantBaseURL: "https://openrouter.ai/api/v1",
		wantModel:   "anthropic/claude-sonnet-4",
		wantKeyEnv:  "OR_KEY",
		wantHas:     true,
		wantCaching: true, // inherited from builtin openrouter profile
	},
	{
		name: "project overrides global active",
		globalJSON: `{
				"profiles": {
					"openrouter": {"model": "gpt-4o", "api_key_env": "OR_KEY"},
					"glm": {"base_url": "https://glm.example/v4", "model": "glm-5", "api_key_env": "GLM_KEY"}
				},
				"active": "openrouter"
			}`,
		projectJSON: `{"active": "glm"}`,
		env:         map[string]string{"GLM_KEY": "glm-secret"},
		wantProfile: "glm",
		wantBaseURL: "https://glm.example/v4",
		wantModel:   "glm-5",
		wantKeyEnv:  "GLM_KEY",
		wantHas:     true,
	},
	{
		name: "project adds new profile",
		globalJSON: `{
				"profiles": {"base": {"model": "gpt-4o"}},
				"active": "base"
			}`,
		projectJSON: `{
				"profiles": {"local": {"base_url": "http://localhost:11434/v1", "model": "llama3", "api_key_env": "LOCAL_KEY"}},
				"active": "local"
			}`,
		env:         map[string]string{"LOCAL_KEY": "unused"},
		wantProfile: "local",
		wantBaseURL: "http://localhost:11434/v1",
		wantModel:   "llama3",
		wantKeyEnv:  "LOCAL_KEY",
		wantHas:     true,
	},
	{
		name: "env vars override profile",
		globalJSON: `{
				"profiles": {"p": {"base_url": "https://example.com", "model": "m1", "api_key_env": "P_KEY"}},
				"active": "p"
			}`,
		env:         map[string]string{"P_KEY": "p-secret", "LLM_MODEL": "override-model", "LLM_BASE_URL": "https://override.com"},
		wantProfile: "p",
		wantBaseURL: "https://override.com",
		wantModel:   "override-model",
		wantKeyEnv:  "P_KEY",
		wantHas:     true,
	},
	{
		name: "LLM_API_KEY overrides api_key_env",
		globalJSON: `{
				"profiles": {"p": {"api_key_env": "CUSTOM_KEY"}},
				"active": "p"
			}`,
		env:         map[string]string{"CUSTOM_KEY": "custom", "LLM_API_KEY": "direct"},
		wantProfile: "p",
		wantBaseURL: DefaultBaseURL,
		wantModel:   DefaultModel,
		wantKeyEnv:  DefaultKeyEnv,
		wantHas:     true,
	},
	{
		name:        "partial project config inherits defaults",
		projectJSON: `{"profiles": {"p": {"model": "tiny"}}, "active": "p"}`,
		env:         map[string]string{"LLM_API_KEY": "key"},
		wantProfile: "p",
		wantBaseURL: DefaultBaseURL,
		wantModel:   "tiny",
		wantKeyEnv:  DefaultKeyEnv,
		wantHas:     true,
	},
	{
		name:        "malformed JSON skipped",
		globalJSON:  `{bad json`,
		env:         map[string]string{"LLM_API_KEY": "key"},
		wantProfile: "env",
		wantBaseURL: DefaultBaseURL,
		wantModel:   DefaultModel,
		wantKeyEnv:  DefaultKeyEnv,
		wantHas:     true,
	},
	{
		name:        "active profile not found warns and uses defaults",
		globalJSON:  `{"active": "nonexistent"}`,
		wantProfile: "env",
		wantBaseURL: DefaultBaseURL,
		wantModel:   DefaultModel,
		wantKeyEnv:  DefaultKeyEnv,
		wantHas:     false,
	},
	{
		name:        "GEMINI_API_KEY auto-fallback with no config",
		env:         map[string]string{"GEMINI_API_KEY": "gem-key"},
		wantProfile: "gemini",
		wantBaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
		wantModel:   "gemini-2.5-flash",
		wantKeyEnv:  "GEMINI_API_KEY",
		wantHas:     true,
	},
	{
		name:        "MINIMAX_API_KEY auto-fallback with no config",
		env:         map[string]string{"MINIMAX_API_KEY": "mm-key"},
		wantProfile: "minimax",
		wantBaseURL: "https://api.minimax.io/v1",
		wantModel:   "MiniMax-M2.7",
		wantKeyEnv:  "MINIMAX_API_KEY",
		wantHas:     true,
	},
	{
		name:        "MINIMAX_API_KEY preferred over OPENROUTER_API_KEY in fallback",
		env:         map[string]string{"MINIMAX_API_KEY": "mm-key", "OPENROUTER_API_KEY": "or-key"},
		wantProfile: "minimax",
		wantBaseURL: "https://api.minimax.io/v1",
		wantModel:   "MiniMax-M2.7",
		wantKeyEnv:  "MINIMAX_API_KEY",
		wantHas:     true,
	},
	{
		name:        "OPENROUTER_API_KEY auto-fallback with no config",
		env:         map[string]string{"OPENROUTER_API_KEY": "or-key"},
		wantProfile: "openrouter",
		wantBaseURL: "https://openrouter.ai/api/v1",
		wantModel:   "google/gemini-2.5-flash",
		wantKeyEnv:  "OPENROUTER_API_KEY",
		wantHas:     true,
		wantCaching: true,
	},
	{
		name:        "GEMINI_API_KEY preferred over MINIMAX_API_KEY in fallback",
		env:         map[string]string{"GEMINI_API_KEY": "gem-key", "MINIMAX_API_KEY": "mm-key"},
		wantProfile: "gemini",
		wantBaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
		wantModel:   "gemini-2.5-flash",
		wantKeyEnv:  "GEMINI_API_KEY",
		wantHas:     true,
	},
	{
		name:        "GEMINI_API_KEY preferred over OPENROUTER_API_KEY in fallback",
		env:         map[string]string{"GEMINI_API_KEY": "gem-key", "OPENROUTER_API_KEY": "or-key"},
		wantProfile: "gemini",
		wantBaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
		wantModel:   "gemini-2.5-flash",
		wantKeyEnv:  "GEMINI_API_KEY",
		wantHas:     true,
	},
	{
		name:        "LLM_API_KEY takes precedence over auto-fallback",
		env:         map[string]string{"LLM_API_KEY": "llm-key", "GEMINI_API_KEY": "gem-key"},
		wantProfile: "env",
		wantBaseURL: DefaultBaseURL,
		wantModel:   DefaultModel,
		wantKeyEnv:  DefaultKeyEnv,
		wantHas:     true,
	},
	{
		name: "file config overrides built-in gemini profile",
		globalJSON: `{
				"profiles": {
					"gemini": {
						"model": "gemini-2.5-pro"
					}
				},
				"active": "gemini"
			}`,
		env:         map[string]string{"GEMINI_API_KEY": "gem-key"},
		wantProfile: "gemini",
		wantBaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
		wantModel:   "gemini-2.5-pro",
		wantKeyEnv:  "GEMINI_API_KEY",
		wantHas:     true,
	},
	{
		name: "fallback keeps LLM_BASE_URL and LLM_MODEL overrides",
		env: map[string]string{
			"GEMINI_API_KEY": "gem-key",
			"LLM_BASE_URL":   "https://override.example/v1",
			"LLM_MODEL":      "override-model",
		},
		wantProfile: "gemini",
		wantBaseURL: "https://override.example/v1",
		wantModel:   "override-model",
		wantKeyEnv:  "GEMINI_API_KEY",
		wantHas:     true,
	},
	{
		name: "fallback uses file-overridden built-in profile when no active profile",
		globalJSON: `{
				"profiles": {
					"gemini": {
						"model": "gemini-2.5-pro"
					}
				}
			}`,
		env:         map[string]string{"GEMINI_API_KEY": "gem-key"},
		wantProfile: "gemini",
		wantBaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
		wantModel:   "gemini-2.5-pro",
		wantKeyEnv:  "GEMINI_API_KEY",
		wantHas:     true,
	},
	{
		name: "prompt_caching enabled via active profile",
		globalJSON: `{
				"profiles": {
					"custom": {
						"model": "test-model",
						"api_key_env": "C_KEY",
						"prompt_caching": true
					}
				},
				"active": "custom"
			}`,
		env:         map[string]string{"C_KEY": "key"},
		wantProfile: "custom",
		wantBaseURL: DefaultBaseURL,
		wantModel:   "test-model",
		wantKeyEnv:  "C_KEY",
		wantHas:     true,
		wantCaching: true,
	},
	{
		name: "prompt_caching explicitly disabled overrides builtin",
		globalJSON: `{
				"profiles": {
					"openrouter": {
						"prompt_caching": false
					}
				},
				"active": "openrouter"
			}`,
		env:         map[string]string{"OPENROUTER_API_KEY": "or-key"},
		wantProfile: "openrouter",
		wantBaseURL: "https://openrouter.ai/api/v1",
		wantModel:   "google/gemini-2.5-flash",
		wantKeyEnv:  "OPENROUTER_API_KEY",
		wantHas:     true,
		wantCaching: false,
	},
	{
		name: "fallback clears prompt_caching from rejected active profile",
		globalJSON: `{
				"profiles": {
					"nocreds": {
						"api_key_env": "MISSING_KEY",
						"prompt_caching": true
					}
				},
				"active": "nocreds"
			}`,
		env:         map[string]string{"GEMINI_API_KEY": "gem-key"},
		wantProfile: "gemini",
		wantBaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
		wantModel:   "gemini-2.5-flash",
		wantKeyEnv:  "GEMINI_API_KEY",
		wantHas:     true,
		wantCaching: false, // gemini has no prompt_caching; must not inherit from rejected profile
	},
	{
		name: "project config enables prompt_caching on profile",
		projectJSON: `{
				"profiles": {
					"gemini": {
						"prompt_caching": true
					}
				},
				"active": "gemini"
			}`,
		env:         map[string]string{"GEMINI_API_KEY": "gem-key"},
		wantProfile: "gemini",
		wantBaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
		wantModel:   "gemini-2.5-flash",
		wantKeyEnv:  "GEMINI_API_KEY",
		wantHas:     true,
		wantCaching: true,
	},
}

func TestResolve(t *testing.T) {
	for _, tt := range resolveTests {
		t.Run(tt.name, func(t *testing.T) {
			globalPath, projectRoot := setupResolveTest(t, tt.globalJSON, tt.projectJSON, tt.env)
			_, got := resolveWithPaths(globalPath, projectRoot)
			assertResolved(t, got, tt)
		})
	}
}

func TestDisplayModel(t *testing.T) {
	tests := []struct {
		model string
		want  string
	}{
		{"google/gemini-2.5-flash", "gemini-2.5-flash"},
		{"anthropic/claude-sonnet-4", "claude-sonnet-4"},
		{"glm-5", "glm-5"},
		{"org/sub/model", "model"},
		{"", ""},
	}
	for _, tt := range tests {
		r := &Resolved{Model: tt.model}
		if got := r.DisplayModel(); got != tt.want {
			t.Errorf("DisplayModel(%q) = %q, want %q", tt.model, got, tt.want)
		}
	}
}

func TestProfileNames(t *testing.T) {
	cfg := &Config{
		Profiles: map[string]Profile{
			"zebra": {},
			"alpha": {},
			"mid":   {},
		},
	}
	names := cfg.ProfileNames()
	want := []string{"alpha", "mid", "zebra"}
	if len(names) != len(want) {
		t.Fatalf("ProfileNames() = %v, want %v", names, want)
	}
	for i, n := range names {
		if n != want[i] {
			t.Errorf("ProfileNames()[%d] = %q, want %q", i, n, want[i])
		}
	}

	empty := &Config{}
	if got := empty.ProfileNames(); got != nil {
		t.Errorf("ProfileNames() on empty = %v, want nil", got)
	}
}

func TestResolveProfile(t *testing.T) {
	cfg := &Config{
		Profiles: map[string]Profile{
			"test": {
				BaseURL:   "https://test.example",
				Model:     "test-model",
				APIKeyEnv: "TEST_KEY",
			},
			"minimal": {},
		},
	}

	t.Run("existing profile", func(t *testing.T) {
		t.Setenv("TEST_KEY", "secret")
		t.Setenv("LLM_API_KEY", "")
		r := ResolveProfile(cfg, "test")
		if r == nil {
			t.Fatal("ResolveProfile returned nil")
		}
		if r.BaseURL != "https://test.example" {
			t.Errorf("BaseURL = %q", r.BaseURL)
		}
		if r.Model != "test-model" {
			t.Errorf("Model = %q", r.Model)
		}
		if r.apiKey != "secret" {
			t.Errorf("APIKey = %q", r.apiKey)
		}
		if r.Profile != "test" {
			t.Errorf("Profile = %q", r.Profile)
		}
	})

	t.Run("minimal profile uses defaults", func(t *testing.T) {
		t.Setenv("LLM_API_KEY", "fallback")
		r := ResolveProfile(cfg, "minimal")
		if r == nil {
			t.Fatal("ResolveProfile returned nil")
		}
		if r.BaseURL != DefaultBaseURL {
			t.Errorf("BaseURL = %q, want default", r.BaseURL)
		}
		if r.Model != DefaultModel {
			t.Errorf("Model = %q, want default", r.Model)
		}
		if r.apiKey != "fallback" {
			t.Errorf("APIKey = %q, want fallback from LLM_API_KEY", r.apiKey)
		}
	})

	t.Run("nonexistent profile", func(t *testing.T) {
		r := ResolveProfile(cfg, "nope")
		if r != nil {
			t.Errorf("ResolveProfile(nope) = %+v, want nil", r)
		}
	})

	t.Run("prompt caching from profile", func(t *testing.T) {
		cacheCfg := &Config{
			Profiles: map[string]Profile{
				"cached": {
					BaseURL:       "https://test.example",
					Model:         "test-model",
					APIKeyEnv:     "TEST_KEY",
					PromptCaching: ptrBool(true),
				},
			},
		}
		t.Setenv("TEST_KEY", "secret")
		t.Setenv("LLM_API_KEY", "")
		r := ResolveProfile(cacheCfg, "cached")
		if r == nil {
			t.Fatal("ResolveProfile returned nil")
		}
		if !r.PromptCaching {
			t.Error("PromptCaching should be true")
		}
	})
}

// setupResolveTest creates temp config files and sets env vars for a resolve test.
func setupResolveTest(t *testing.T, globalJSON, projectJSON string, env map[string]string) (globalPath, projectRoot string) {
	t.Helper()
	for _, key := range []string{"LLM_API_KEY", "LLM_BASE_URL", "LLM_MODEL"} {
		t.Setenv(key, "")
	}
	// Clear built-in profile env vars so auto-fallback doesn't trigger unexpectedly.
	for _, p := range builtinProfiles {
		if p.APIKeyEnv != "" {
			t.Setenv(p.APIKeyEnv, "")
		}
	}
	for key, val := range env {
		t.Setenv(key, val)
	}

	tmpDir := t.TempDir()
	globalDir := filepath.Join(tmpDir, "global", "junto")
	if globalJSON != "" {
		if err := os.MkdirAll(globalDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(globalDir, "llm.json"), []byte(globalJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	projectRoot = filepath.Join(tmpDir, "project")
	if projectJSON != "" {
		projDir := filepath.Join(projectRoot, ".project")
		if err := os.MkdirAll(projDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(projDir, "llm.json"), []byte(projectJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(globalDir, "llm.json"), projectRoot
}

// assertResolved checks all fields of a Resolved value.
func assertResolved(t *testing.T, got *Resolved, tc resolveTestCase) {
	t.Helper()
	if got.Profile != tc.wantProfile {
		t.Errorf("Profile = %q, want %q", got.Profile, tc.wantProfile)
	}
	if got.BaseURL != tc.wantBaseURL {
		t.Errorf("BaseURL = %q, want %q", got.BaseURL, tc.wantBaseURL)
	}
	if got.Model != tc.wantModel {
		t.Errorf("Model = %q, want %q", got.Model, tc.wantModel)
	}
	if got.APIKeyEnv != tc.wantKeyEnv {
		t.Errorf("APIKeyEnv = %q, want %q", got.APIKeyEnv, tc.wantKeyEnv)
	}
	if got.HasProvider() != tc.wantHas {
		t.Errorf("HasProvider() = %v, want %v", got.HasProvider(), tc.wantHas)
	}
	if got.PromptCaching != tc.wantCaching {
		t.Errorf("PromptCaching = %v, want %v", got.PromptCaching, tc.wantCaching)
	}
}
