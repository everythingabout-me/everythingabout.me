package config

import (
	"os"
	"strings"
	"testing"
)

// APP_ENV chooses between the development shortcuts (verification codes in the response,
// non-Secure cookies) and production behavior. A missing value used to mean development,
// so any host whose operator forgot the line served development shortcuts silently; the
// contract is now that Load refuses instead of choosing for them.
func TestAppEnvMustBeExplicit(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		isolatedConfigEnv(t)
		os.Unsetenv("APP_ENV")
		_, err := Load()
		if err == nil {
			t.Fatal("Load accepted a missing APP_ENV instead of refusing to start")
		}
		message := err.Error()
		if !strings.Contains(message, "APP_ENV") || !strings.Contains(message, EnvDevelopment) || !strings.Contains(message, EnvProduction) {
			t.Fatalf("missing APP_ENV error does not name the variable and both accepted values: %q", message)
		}
	})

	t.Run("empty", func(t *testing.T) {
		isolatedConfigEnv(t)
		t.Setenv("APP_ENV", "  ")
		if _, err := Load(); err == nil {
			t.Fatal("Load treated a blank APP_ENV as a valid environment")
		}
	})

	t.Run("invalid", func(t *testing.T) {
		isolatedConfigEnv(t)
		t.Setenv("APP_ENV", "staging")
		_, err := Load()
		if err == nil {
			t.Fatal("Load accepted an unrecognized APP_ENV")
		}
		message := err.Error()
		if !strings.Contains(message, "staging") || !strings.Contains(message, EnvDevelopment) || !strings.Contains(message, EnvProduction) {
			t.Fatalf("invalid APP_ENV error does not report the value and the accepted set: %q", message)
		}
	})

	t.Run("development", func(t *testing.T) {
		isolatedConfigEnv(t)
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.AppEnv != EnvDevelopment || cfg.IsProduction() || cfg.CookieSecure {
			t.Fatalf("development did not keep the development behavior: %q production=%v secure=%v", cfg.AppEnv, cfg.IsProduction(), cfg.CookieSecure)
		}
	})

	t.Run("production", func(t *testing.T) {
		isolatedConfigEnv(t)
		t.Setenv("APP_ENV", EnvProduction)
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.AppEnv != EnvProduction || !cfg.IsProduction() || !cfg.CookieSecure {
			t.Fatalf("production did not keep the production behavior: %q production=%v secure=%v", cfg.AppEnv, cfg.IsProduction(), cfg.CookieSecure)
		}
	})
}

// APP_ENV has no default, so the documented local entry point has to supply it or
// `make dev` stops before listening and the login page never receives a code. This
// asserts the recipe, not just the documentation: removing the assignment is exactly the
// change that would break a fresh clone without failing any other test.
func TestMakeDevSuppliesDevelopmentEnvironment(t *testing.T) {
	data, err := os.ReadFile("../../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	recipe := devRecipe(t, string(data))
	if !strings.Contains(recipe, "APP_ENV") || !strings.Contains(recipe, EnvDevelopment) {
		t.Fatalf("make dev does not set APP_ENV=%s for the backend, so it cannot start without a .env value:\n%s", EnvDevelopment, recipe)
	}
}

// devRecipe returns the tab-indented recipe lines of the dev target.
func devRecipe(t *testing.T, makefile string) string {
	t.Helper()
	lines := strings.Split(makefile, "\n")
	var recipe []string
	found := false
	for _, line := range lines {
		if !found {
			if strings.HasPrefix(line, "dev:") {
				found = true
			}
			continue
		}
		if !strings.HasPrefix(line, "\t") {
			break
		}
		recipe = append(recipe, line)
	}
	if !found {
		t.Fatal("Makefile has no dev target")
	}
	return strings.Join(recipe, "\n")
}
