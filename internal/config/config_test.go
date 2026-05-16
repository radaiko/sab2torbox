package config

import (
	"os"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SAB2TORBOX_TORBOX_API_TOKEN", "tok")
	t.Setenv("SAB2TORBOX_SAB_API_KEY", "key")
	t.Setenv("SAB2TORBOX_WEBDAV_MOUNT_ROOT", dir)
	t.Setenv("SAB2TORBOX_SYMLINK_ROOT", dir)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ListenAddr != ":8080" {
		t.Errorf("ListenAddr default: %q", c.ListenAddr)
	}
	if c.PollInterval != time.Minute {
		t.Errorf("PollInterval default: %v", c.PollInterval)
	}
	if c.UsenetPath() != dir {
		t.Errorf("UsenetPath default should be the mount root: %q", c.UsenetPath())
	}
	if len(c.Categories) != 3 {
		t.Errorf("Categories default: %v", c.Categories)
	}
	if c.TorBoxWebDAVRefreshURL != "https://webdav.torbox.app/refresh" {
		t.Errorf("refresh URL default: %q", c.TorBoxWebDAVRefreshURL)
	}
	if c.WebDAVRefreshCooldown != 2*time.Minute {
		t.Errorf("refresh cooldown default: %v", c.WebDAVRefreshCooldown)
	}
	if c.WebDAVRefreshEnabled() {
		t.Error("WebDAV refresh must be disabled without credentials")
	}
}

func TestLoadRequiresSymlinkRoot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SAB2TORBOX_TORBOX_API_TOKEN", "t")
	t.Setenv("SAB2TORBOX_SAB_API_KEY", "k")
	t.Setenv("SAB2TORBOX_WEBDAV_MOUNT_ROOT", dir)

	// SYMLINK_ROOT is required.
	os.Unsetenv("SAB2TORBOX_SYMLINK_ROOT")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when SYMLINK_ROOT is unset")
	}
	// Set but nonexistent — must fail validation.
	t.Setenv("SAB2TORBOX_SYMLINK_ROOT", "/nonexistent/symlink/root/xyz")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for a missing symlink root directory")
	}
	// Valid.
	t.Setenv("SAB2TORBOX_SYMLINK_ROOT", dir)
	if _, err := Load(); err != nil {
		t.Fatalf("Load with a valid symlink root: %v", err)
	}
}

func TestWebDAVRefreshEnabled(t *testing.T) {
	if !(&Config{TorBoxWebDAVUser: "u", TorBoxWebDAVPass: "p"}).WebDAVRefreshEnabled() {
		t.Error("should be enabled when both credentials are set")
	}
	if (&Config{TorBoxWebDAVUser: "u"}).WebDAVRefreshEnabled() {
		t.Error("should be disabled when password is missing")
	}
	if (&Config{TorBoxWebDAVPass: "p"}).WebDAVRefreshEnabled() {
		t.Error("should be disabled when user is missing")
	}
}

func TestLoadMissingRequired(t *testing.T) {
	os.Clearenv()
	if _, err := Load(); err == nil {
		t.Fatal("expected error for missing required vars")
	}
}

func TestSlogLevel(t *testing.T) {
	cases := map[string]string{"debug": "DEBUG", "warn": "WARN", "error": "ERROR", "info": "INFO", "": "INFO"}
	for in, want := range cases {
		if got := (&Config{LogLevel: in}).SlogLevel().String(); got != want {
			t.Errorf("SlogLevel(%q): got %s want %s", in, got, want)
		}
	}
}

func TestAllowsCategory(t *testing.T) {
	c := &Config{Categories: []string{"sonarr", "radarr"}}
	if !c.AllowsCategory("sonarr") {
		t.Error("sonarr should be allowed")
	}
	if c.AllowsCategory("lidarr") {
		t.Error("lidarr should not be allowed")
	}
}

func TestValidateMountNotDir(t *testing.T) {
	f := t.TempDir() + "/afile"
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (&Config{WebDAVMountRoot: f}).validateMount(); err == nil {
		t.Error("expected error when mount root is a file")
	}
}

func TestValidateMountMissing(t *testing.T) {
	c := &Config{WebDAVMountRoot: "/nonexistent/path/xyz"}
	if err := c.validateMount(); err == nil {
		t.Fatal("expected error for missing mount root")
	}
}

func TestHealConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SAB2TORBOX_TORBOX_API_TOKEN", "t")
	t.Setenv("SAB2TORBOX_SAB_API_KEY", "k")
	t.Setenv("SAB2TORBOX_WEBDAV_MOUNT_ROOT", dir)
	t.Setenv("SAB2TORBOX_SYMLINK_ROOT", dir)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.HealEnabled {
		t.Error("HealEnabled must default to false")
	}
	if c.HealInterval != time.Hour {
		t.Errorf("HealInterval default: %v", c.HealInterval)
	}
	if c.HealMaxAttempts != 3 {
		t.Errorf("HealMaxAttempts default: %d", c.HealMaxAttempts)
	}
	if c.HealBackoffInitial != 5*time.Minute {
		t.Errorf("HealBackoffInitial default: %v", c.HealBackoffInitial)
	}
}

func TestHealRequiresLibraryRootsWhenEnabled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SAB2TORBOX_TORBOX_API_TOKEN", "t")
	t.Setenv("SAB2TORBOX_SAB_API_KEY", "k")
	t.Setenv("SAB2TORBOX_WEBDAV_MOUNT_ROOT", dir)
	t.Setenv("SAB2TORBOX_SYMLINK_ROOT", dir)
	t.Setenv("SAB2TORBOX_HEAL_ENABLED", "true")

	if _, err := Load(); err == nil {
		t.Fatal("expected error: HEAL_ENABLED without HEAL_LIBRARY_ROOTS")
	}
	t.Setenv("SAB2TORBOX_HEAL_LIBRARY_ROOTS", dir)
	if _, err := Load(); err != nil {
		t.Fatalf("Load with valid heal config: %v", err)
	}
}

func TestHealRejectsZeroMaxAttempts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SAB2TORBOX_TORBOX_API_TOKEN", "t")
	t.Setenv("SAB2TORBOX_SAB_API_KEY", "k")
	t.Setenv("SAB2TORBOX_WEBDAV_MOUNT_ROOT", dir)
	t.Setenv("SAB2TORBOX_SYMLINK_ROOT", dir)
	t.Setenv("SAB2TORBOX_HEAL_ENABLED", "true")
	t.Setenv("SAB2TORBOX_HEAL_LIBRARY_ROOTS", dir)
	t.Setenv("SAB2TORBOX_HEAL_MAX_ATTEMPTS", "0")
	if _, err := Load(); err == nil {
		t.Fatal("expected error: HEAL_MAX_ATTEMPTS=0 means nothing ever heals")
	}
}

func TestHealWebhookDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SAB2TORBOX_TORBOX_API_TOKEN", "t")
	t.Setenv("SAB2TORBOX_SAB_API_KEY", "k")
	t.Setenv("SAB2TORBOX_WEBDAV_MOUNT_ROOT", dir)
	t.Setenv("SAB2TORBOX_SYMLINK_ROOT", dir)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.HealWebhookURL != "" {
		t.Errorf("HealWebhookURL default should be empty, got %q", c.HealWebhookURL)
	}
	if len(c.HealWebhookEvents) != 1 || c.HealWebhookEvents[0] != "failed" {
		t.Errorf("HealWebhookEvents default should be [failed], got %v", c.HealWebhookEvents)
	}
}
