package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCoreMediaCopy(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	file := filepath.Join(source, "fragment")
	if err := os.WriteFile(file, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := copyCoreMedia(source, target); err != nil {
			t.Fatal(err)
		}
	}
	if err := copyCoreMedia(source, filepath.Join(source, "nested")); err == nil {
		t.Fatal("nested target accepted")
	}
	if err := os.WriteFile(filepath.Join(target, "fragment"), []byte("different"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := copyCoreMedia(source, target); err == nil {
		t.Fatal("conflict accepted")
	}
	got, _ := os.ReadFile(file)
	if string(got) != "original" {
		t.Fatal("source modified")
	}
	t.Setenv("SOURCE_DATA_DIR", source)
	t.Setenv("TARGET_DATA_DIR", target)
	gotPath, err := migratedUploadPath(file)
	if err != nil || gotPath != filepath.Join(target, "fragment") {
		t.Fatalf("path mapping: %s %v", gotPath, err)
	}
	if _, err := migratedUploadPath(filepath.Join(source, "..", "outside")); err == nil {
		t.Fatal("outside path accepted")
	}
}
