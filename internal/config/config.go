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
	WebDAVUsenetSubpath string        `envconfig:"WEBDAV_USENET_SUBPATH"`
	SymlinkRoot         string        `envconfig:"SYMLINK_ROOT" required:"true"`
	ListenAddr          string        `envconfig:"LISTEN_ADDR" default:":8080"`
	DatabasePath        string        `envconfig:"DATABASE_PATH" default:"/config/sab2torbox.db"`
	PollInterval        time.Duration `envconfig:"POLL_INTERVAL" default:"1m"`
	LogLevel            string        `envconfig:"LOG_LEVEL" default:"info"`
	Categories          []string      `envconfig:"CATEGORIES" default:"sonarr,radarr,sonarr-anime"`

	// TorBox WebDAV /refresh integration. TorBox's WebDAV listing only
	// refreshes every 15 minutes; hitting /refresh forces it sooner. The
	// feature is active only when both credentials are set.
	TorBoxWebDAVUser       string        `envconfig:"TORBOX_WEBDAV_USER"`
	TorBoxWebDAVPass       string        `envconfig:"TORBOX_WEBDAV_PASS"`
	TorBoxWebDAVRefreshURL string        `envconfig:"TORBOX_WEBDAV_REFRESH_URL" default:"https://webdav.torbox.app/refresh"`
	WebDAVRefreshCooldown  time.Duration `envconfig:"TORBOX_WEBDAV_REFRESH_COOLDOWN" default:"2m"`

	HealEnabled        bool          `envconfig:"HEAL_ENABLED" default:"false"`
	HealInterval       time.Duration `envconfig:"HEAL_INTERVAL" default:"1h"`
	HealLibraryRoots   []string      `envconfig:"HEAL_LIBRARY_ROOTS"`
	HealDryRun         bool          `envconfig:"HEAL_DRY_RUN" default:"false"`
	HealMaxAttempts    int           `envconfig:"HEAL_MAX_ATTEMPTS" default:"3"`
	HealBackoffInitial time.Duration `envconfig:"HEAL_BACKOFF_INITIAL" default:"5m"`
	HealWebhookURL     string        `envconfig:"HEAL_WEBHOOK_URL"`
	HealWebhookEvents  []string      `envconfig:"HEAL_WEBHOOK_EVENTS" default:"failed"`
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
	symInfo, err := os.Stat(c.SymlinkRoot)
	if err != nil {
		return nil, fmt.Errorf("symlink root %q: %w", c.SymlinkRoot, err)
	}
	if !symInfo.IsDir() {
		return nil, fmt.Errorf("symlink root %q is not a directory", c.SymlinkRoot)
	}
	if c.HealEnabled {
		if len(c.HealLibraryRoots) == 0 {
			return nil, fmt.Errorf("HEAL_ENABLED requires HEAL_LIBRARY_ROOTS")
		}
		for _, root := range c.HealLibraryRoots {
			info, err := os.Stat(root)
			if err != nil {
				return nil, fmt.Errorf("heal library root %q: %w", root, err)
			}
			if !info.IsDir() {
				return nil, fmt.Errorf("heal library root %q is not a directory", root)
			}
		}
		if c.HealMaxAttempts <= 0 {
			return nil, fmt.Errorf("HEAL_MAX_ATTEMPTS must be greater than 0")
		}
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

// UsenetPath returns the host filesystem path under which TorBox creates one
// folder per completed Usenet download. With an empty WebDAVUsenetSubpath
// (the default) this is the mount root itself, which matches TorBox's WebDAV
// layout — TorBox does not nest releases under a "usenet" folder.
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

// WebDAVRefreshEnabled reports whether the TorBox WebDAV /refresh integration
// is configured. It activates only when both WebDAV credentials are present.
func (c *Config) WebDAVRefreshEnabled() bool {
	return c.TorBoxWebDAVUser != "" && c.TorBoxWebDAVPass != ""
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
