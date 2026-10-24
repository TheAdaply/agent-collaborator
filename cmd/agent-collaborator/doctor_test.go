package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorMissingConfigDoesNotCreateState(t *testing.T) {
	home := filepath.Join(t.TempDir(), "absent")
	t.Setenv("AC_HOME", home)
	t.Setenv("PATH", t.TempDir())
	var out bytes.Buffer
	if err := doctor(context.Background(), &out); err == nil {
		t.Fatal("missing setup should fail")
	}
	for _, want := range []string{"config FAIL", "daemon FAIL", "tailnet FAIL", "engines FAIL", "service "} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in %s", want, &out)
		}
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("doctor created state: %v", err)
	}
}

func TestDoctorInvalidConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AC_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(`network = "invalid"`), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := doctor(context.Background(), &out); err == nil || !strings.Contains(out.String(), "config FAIL") {
		t.Fatalf("%v: %s", err, &out)
	}
	if _, err := os.Stat(filepath.Join(home, "state")); !os.IsNotExist(err) {
		t.Fatalf("doctor created state: %v", err)
	}
}
