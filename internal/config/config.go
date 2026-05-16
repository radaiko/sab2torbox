// Package config loads and validates sab2torbox configuration from the environment.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/kelseyhightower/envconfig"
)

// Config holds all runtime configuration, populated from SAB2TORBOX_* env vars.
type Config struct {
	TorBoxAPIToken      string        `envconfig:"TORBOX_API_TOKEN" required:"true"`
	SABAPIKey           string        `envconfig:"SAB_API_KEY" required:"true"`
	WebDAVMountRoot     string        `envconfig:"WEBDAV_MOUNT_ROOT" required:"true"`
	WebDAVUsenetSubpath string        `envconfig:"WEBDAV_USENET_SUBPATH" default:"usenet"`
	ListenAddr          string        `envconfig:"LISTEN_ADDR" default:":8080"`
	DatabasePath        string        `envconfig:"DATABASE_PATH" default:"/config/sab2torbox.db"`
	PollInterval        time.Duration `envconfig:"POLL_INTERVAL" default:"10s"`
	LogLevel            string        `envconfig:"LOG_LEVEL" default:"info"`
	Categories          []string      `envconfig:"CATEGORIES" default:"sonarr,radarr,sonarr-anime"`
}

// Load reads configuration from the environment and validates it.
func Load() (*Config, error) {
	var c Config
	if err := envconfig.Process("sab2torbox", &c); err != nil {
		return nil, fmt.Errorf("processing env config: %w", err)
	}
	if err := c.validateMount(); err != nil {
		return nil, err
	}
	return &c, nil
}

// validateMount ensures the WebDAV mount root exists and is a directory.
func (c *Config) validateMount() error {
	info, err := os.Stat(c.WebDAVMountRoot)
	if err != nil {
		return fmt.Errorf("webdav mount root %q: %w", c.WebDAVMountRoot, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("webdav mount root %q is not a directory", c.WebDAVMountRoot)
	}
	return nil
}

// UsenetPath returns the host filesystem path where TorBox Usenet downloads appear.
func (c *Config) UsenetPath() string {
	return filepath.Join(c.WebDAVMountRoot, c.WebDAVUsenetSubpath)
}

// SlogLevel maps the configured log level string to a slog.Level.
func (c *Config) SlogLevel() slog.Level {
	switch c.LogLevel {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// AllowsCategory reports whether cat is in the configured category allowlist.
func (c *Config) AllowsCategory(cat string) bool {
	for _, allowed := range c.Categories {
		if allowed == cat {
			return true
		}
	}
	return false
}
