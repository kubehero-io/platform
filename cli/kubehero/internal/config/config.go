// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

// Package config loads and persists CLI settings.
//
// Resolution order (later wins):
//   1. ~/.kubehero/config.yaml
//   2. KUBEHERO_ENDPOINT, KUBEHERO_TOKEN, KUBEHERO_ORG env vars
//   3. --endpoint, --token, --org flags

package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Endpoint string `yaml:"endpoint"`           // e.g. https://api.kubehero.io
	Token    string `yaml:"token,omitempty"`    // bearer token; do not log
	Org      string `yaml:"org,omitempty"`      // optional default org slug
	Output   string `yaml:"output,omitempty"`   // table · json · yaml · wide
	Insecure bool   `yaml:"insecure,omitempty"` // skip TLS verify
}

const dirName = ".kubehero"
const fileName = "config.yaml"

func path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, dirName, fileName), nil
}

func Load() (*Config, error) {
	c := &Config{Output: "table"}

	if p, err := path(); err == nil {
		if b, err := os.ReadFile(p); err == nil {
			_ = yaml.Unmarshal(b, c)
		}
	}

	if v := os.Getenv("KUBEHERO_ENDPOINT"); v != "" {
		c.Endpoint = v
	}
	if v := os.Getenv("KUBEHERO_TOKEN"); v != "" {
		c.Token = v
	}
	if v := os.Getenv("KUBEHERO_ORG"); v != "" {
		c.Org = v
	}
	if c.Output == "" {
		c.Output = "table"
	}
	return c, nil
}

func Save(c *Config) error {
	p, err := path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", p, err)
	}
	return nil
}
