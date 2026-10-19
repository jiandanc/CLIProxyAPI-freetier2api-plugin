package app

import (
	"os"
	"path/filepath"
	"testing"

	"freetier2api-plugin/internal/admin"
)

func TestMigrateLegacyStateDir(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	legacyDir := filepath.Join(tempHome, ".cli-proxy-api", "freetier2api")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatalf("failed to create legacy dir: %v", err)
	}

	legacyStateFile := filepath.Join(legacyDir, admin.StateFileName)
	legacySaltFile := filepath.Join(legacyDir, admin.SaltFileName)
	if err := os.WriteFile(legacyStateFile, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatalf("failed to write legacy state: %v", err)
	}
	if err := os.WriteFile(legacySaltFile, []byte("salt123"), 0o600); err != nil {
		t.Fatalf("failed to write legacy salt: %v", err)
	}

	targetDir := filepath.Join(tempHome, ".freetier2api-plugin")
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		t.Fatalf("failed to create target dir: %v", err)
	}

	migrateLegacyStateDir(targetDir)

	targetStateFile := filepath.Join(targetDir, admin.StateFileName)
	targetSaltFile := filepath.Join(targetDir, admin.SaltFileName)

	if data, err := os.ReadFile(targetStateFile); err != nil || string(data) != `{"version":1}` {
		t.Fatalf("target state file not migrated properly: %v, content: %s", err, string(data))
	}
	if data, err := os.ReadFile(targetSaltFile); err != nil || string(data) != "salt123" {
		t.Fatalf("target salt file not migrated properly: %v, content: %s", err, string(data))
	}

	if _, err := os.Stat(legacyStateFile); !os.IsNotExist(err) {
		t.Fatalf("legacy state file was not removed: %v", err)
	}
	if _, err := os.Stat(legacySaltFile); !os.IsNotExist(err) {
		t.Fatalf("legacy salt file was not removed: %v", err)
	}
}
