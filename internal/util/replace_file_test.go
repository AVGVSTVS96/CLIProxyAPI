package util

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReplaceFileSkipsUnchangedData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "run.json")
	if err := ReplaceFile(path, []byte("a")); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFile(path, []byte("a")); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); !info.ModTime().Equal(old) {
		t.Fatalf("unchanged data rewrote the file")
	}
	if err := ReplaceFile(path, []byte("b")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "b" {
		t.Fatalf("file = %q, want b", got)
	}
}
