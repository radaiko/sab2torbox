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
	if c.UsenetPath() != dir+"/usenet" {
		t.Errorf("UsenetPath: %q", c.UsenetPath())
	}
	if len(c.Categories) != 3 {
		t.Errorf("Categories default: %v", c.Categories)
	}
}

func TestLoadMissingRequired(t *testing.T) {
	os.Clearenv()
	if _, err := Load(); err == nil {
		t.Fatal("expected error for missing required vars")
	}
}

func TestValidateMountMissing(t *testing.T) {
	c := &Config{WebDAVMountRoot: "/nonexistent/path/xyz"}
	if err := c.validateMount(); err == nil {
		t.Fatal("expected error for missing mount root")
	}
}
