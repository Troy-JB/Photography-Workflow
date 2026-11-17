// Package appdir resolves the per-user locations for agent state, settings and
// logs. PWF_AGENT_DATA relocates everything (used by tests and CI).
package appdir

import (
	"os"
	"path/filepath"
)

func DataDir() string {
	if p := os.Getenv("PWF_AGENT_DATA"); p != "" {
		return p
	}
	if dir := os.Getenv("LOCALAPPDATA"); dir != "" {
		return filepath.Join(dir, "PWF", "agent")
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "pwf-agent")
	}
	return "."
}

func DBPath() string {
	if p := os.Getenv("PWF_AGENT_DB"); p != "" {
		return p
	}
	return filepath.Join(DataDir(), "state.db")
}

func SettingsPath() string {
	return filepath.Join(DataDir(), "settings.json")
}

func LogDir() string {
	return filepath.Join(DataDir(), "logs")
}

func CacheDir() string {
	return filepath.Join(DataDir(), "cache")
}
