// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// isolateHome points $HOME at a fresh temp dir and clears KUBEHERO_* env
// vars so Load only sees what the test sets up.
func isolateHome(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("HOME-based isolation is unix-only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KUBEHERO_ENDPOINT", "")
	t.Setenv("KUBEHERO_TOKEN", "")
	t.Setenv("KUBEHERO_ORG", "")
	return home
}

func TestLoadMissingFileDefaults(t *testing.T) {
	isolateHome(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Output: "table"}
	if *c != want {
		t.Errorf("Load() = %+v, want %+v", *c, want)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{
			name: "full config",
			cfg: Config{
				Endpoint: "https://api.kubehero.io",
				Token:    "tok-abc",
				Org:      "acme",
				Output:   "json",
				Insecure: true,
			},
		},
		{
			name: "minimal config",
			cfg:  Config{Endpoint: "http://localhost:8080", Output: "table"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateHome(t)
			if err := Save(&tt.cfg); err != nil {
				t.Fatal(err)
			}
			got, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if *got != tt.cfg {
				t.Errorf("round-trip = %+v, want %+v", *got, tt.cfg)
			}
		})
	}
}

func TestSaveFilePermissions(t *testing.T) {
	home := isolateHome(t)
	if err := Save(&Config{Endpoint: "https://api.kubehero.io", Token: "secret"}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(home, ".kubehero", "config.yaml")

	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("config file mode = %o, want 600 (holds the token)", perm)
	}
	di, err := os.Stat(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("config dir mode = %o, want 700", perm)
	}
}

// Load deliberately tolerates a corrupt file: it must not brick the CLI, so
// it falls back to defaults instead of returning an error.
func TestLoadMalformedFileFallsBackToDefaults(t *testing.T) {
	home := isolateHome(t)
	dir := filepath.Join(home, ".kubehero")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(":\n\t{not yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil (corrupt file should degrade to defaults)", err)
	}
	want := Config{Output: "table"}
	if *c != want {
		t.Errorf("Load() = %+v, want defaults %+v", *c, want)
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	tests := []struct {
		name string
		file *Config // nil = no file on disk
		env  map[string]string
		want Config
	}{
		{
			name: "env wins over file",
			file: &Config{Endpoint: "https://file.example.com", Token: "file-tok", Org: "file-org", Output: "yaml"},
			env: map[string]string{
				"KUBEHERO_ENDPOINT": "https://env.example.com",
				"KUBEHERO_TOKEN":    "env-tok",
				"KUBEHERO_ORG":      "env-org",
			},
			want: Config{Endpoint: "https://env.example.com", Token: "env-tok", Org: "env-org", Output: "yaml"},
		},
		{
			name: "partial env keeps file values",
			file: &Config{Endpoint: "https://file.example.com", Token: "file-tok", Output: "wide"},
			env:  map[string]string{"KUBEHERO_TOKEN": "env-tok"},
			want: Config{Endpoint: "https://file.example.com", Token: "env-tok", Output: "wide"},
		},
		{
			name: "env alone with no file",
			env:  map[string]string{"KUBEHERO_ENDPOINT": "https://env-only.example.com"},
			want: Config{Endpoint: "https://env-only.example.com", Output: "table"},
		},
		{
			name: "empty env var does not clear file value",
			file: &Config{Endpoint: "https://file.example.com"},
			env:  map[string]string{"KUBEHERO_ENDPOINT": ""},
			want: Config{Endpoint: "https://file.example.com", Output: "table"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateHome(t)
			if tt.file != nil {
				if err := Save(tt.file); err != nil {
					t.Fatal(err)
				}
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			got, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if *got != tt.want {
				t.Errorf("Load() = %+v, want %+v", *got, tt.want)
			}
		})
	}
}

func TestLoadFileWithEmptyOutputDefaultsToTable(t *testing.T) {
	isolateHome(t)
	if err := Save(&Config{Endpoint: "https://api.kubehero.io"}); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Output != "table" {
		t.Errorf("Output = %q, want table default", c.Output)
	}
}
