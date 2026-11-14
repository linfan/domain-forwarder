package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveRequestFiles(t *testing.T) {
	dir := t.TempDir()

	name, err := saveRequestFiles(dir, "20260101-000000.000000", "GET / HTTP/1.1\n\n", []byte("hello"))
	if err != nil {
		t.Fatalf("saveRequestFiles: %v", err)
	}
	if name != "20260101-000000.000000" {
		t.Fatalf("name = %q, want base name", name)
	}

	headers, err := os.ReadFile(filepath.Join(dir, name+".header"))
	if err != nil {
		t.Fatalf("read header: %v", err)
	}
	if string(headers) != "GET / HTTP/1.1\n\n" {
		t.Fatalf("header content = %q", headers)
	}

	body, err := os.ReadFile(filepath.Join(dir, name+".body"))
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "hello" {
		t.Fatalf("body content = %q", body)
	}

	name, err = saveRequestFiles(dir, "20260101-000000.000000", "GET /2 HTTP/1.1\n\n", nil)
	if err != nil {
		t.Fatalf("saveRequestFiles collision: %v", err)
	}
	if name != "20260101-000000.000000-2" {
		t.Fatalf("collision name = %q, want -2 suffix", name)
	}
}
