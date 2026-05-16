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

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ListenAddr != ":8080" {
		t.Errorf("ListenAddr default: %q", c.ListenAddr)
	}
	if c.PollInterval != 10*time.Second {
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
