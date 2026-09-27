package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotenv(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(path, []byte(`
# comment
HUNCH_A=plain
export HUNCH_B="quoted value"
HUNCH_C='single'
HUNCH_D=already
`), 0o600)
	t.Setenv("HUNCH_D", "from env")
	for _, k := range []string{"HUNCH_A", "HUNCH_B", "HUNCH_C"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}

	if err := loadDotenv(path); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"HUNCH_A": "plain", "HUNCH_B": "quoted value", "HUNCH_C": "single", "HUNCH_D": "from env"}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}

	if err := loadDotenv(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Errorf("missing file: %v", err)
	}
}
