package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

const (
	// DefaultConfigPath is the well-known path where a ConfigMap is mounted.
	// If this file exists, it overrides embedded platform defaults.
	DefaultConfigPath = "/etc/rhaii-validate/platform.yaml"
)

// Load returns a PlatformConfig by:
// 1. Loading embedded defaults for the detected/given platform
// 2. Checking for a config file at the well-known path (ConfigMap mount)
// 3. If configFile is explicitly provided, using that instead
// Only provided fields in the override file replace the defaults.
func Load(platform Platform, configFile string) (PlatformConfig, error) {
	cfg, err := GetConfig(platform)
	if err != nil {
		return PlatformConfig{}, err
	}

	// Determine which override file to use
	overrideFile := configFile
	if overrideFile == "" {
		// Check well-known ConfigMap mount path
		if _, err := os.Stat(DefaultConfigPath); err == nil {
			overrideFile = DefaultConfigPath
		}
	}

	if overrideFile == "" {
		return cfg, nil
	}

	data, err := os.ReadFile(overrideFile)
	if err != nil {
		return cfg, fmt.Errorf("failed to read config file %s: %w", overrideFile, err)
	}

	// Unmarshal on top of defaults — only provided fields are overridden
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("failed to parse config file %s: %w", overrideFile, err)
	}

	return cfg, nil
}
