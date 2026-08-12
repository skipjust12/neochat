package envfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_SetsUnsetVars(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("FOO=bar\n# comment\n\nBAZ=\"quoted\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("FOO")
	os.Unsetenv("BAZ")
	t.Cleanup(func() {
		os.Unsetenv("FOO")
		os.Unsetenv("BAZ")
	})

	if err := Load(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("FOO"); got != "bar" {
		t.Errorf("FOO = %q, want %q", got, "bar")
	}
	if got := os.Getenv("BAZ"); got != "quoted" {
		t.Errorf("BAZ = %q, want %q", got, "quoted")
	}
}

func TestLoad_DoesNotOverwriteExistingEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("FOO=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Setenv("FOO", "from-real-env")
	t.Cleanup(func() { os.Unsetenv("FOO") })

	if err := Load(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("FOO"); got != "from-real-env" {
		t.Errorf("FOO = %q, want unchanged %q", got, "from-real-env")
	}
}

func TestLoad_MissingFileIsNotAnError(t *testing.T) {
	if err := Load(filepath.Join(t.TempDir(), "does-not-exist.env")); err != nil {
		t.Fatalf("Load on missing file: %v", err)
	}
}
