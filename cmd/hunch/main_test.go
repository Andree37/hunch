package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTestRejectsUnknownNames(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.yaml"), []byte("nodes: {a: {action: log, message: \"{{who}}\"}}\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "f.tests"), 0o755)
	os.WriteFile(filepath.Join(dir, "f.tests", "meeting.yaml"), []byte("input: {who: ann}\n"), 0o644)
	err := cmdTest(context.Background(), []string{filepath.Join(dir, "f.yaml"), "--model", "mock", "meeting", "meetnig"})
	if err == nil || !strings.Contains(err.Error(), `no test named "meetnig"`) || !strings.Contains(err.Error(), "meeting") {
		t.Errorf("err = %v", err)
	}
}

func TestFlagsApplyBeforeTheFlowIsChecked(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "hunch.yaml"), []byte("models: {jev: https://api.typesafe.ai/v1/systemone}\n"), 0o644)
	path := filepath.Join(dir, "f.yaml")
	os.WriteFile(path, []byte("nodes: {draft: {action: llm, prompt: hi}}\n"), 0o644)
	if _, err := loadChecked(path, "", "", os.Stderr); err == nil {
		t.Error("jev can't write, so the defaults should fail")
	}
	if _, err := loadChecked(path, "", "mock", os.Stderr); err != nil {
		t.Errorf("--writer mock should fix it: %v", err)
	}
}
