package musicengine

import (
	"os"
	"path/filepath"
	"testing"
)

// slskd on Beast runs as root and leaves 755 folders, so this process may read a finished
// download but not move it. Adoption then copies it whole into staging and leaves the original.
func TestAdoptCopiesWhenTheClientFolderIsNotWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the folder permission this test relies on")
	}
	root := t.TempDir()
	client := filepath.Join(root, "complete", "Album")
	stage := filepath.Join(root, "staging", "1")
	for _, d := range []string{client, stage} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	src := filepath.Join(client, "01 Song.flac")
	if err := os.WriteFile(src, []byte("fLaC-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(client, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(client, 0o755) })

	dst, copied, err := acqAdopt(src, stage)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if !copied {
		t.Fatal("expected a copy: the client folder is not writable")
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "fLaC-bytes" {
		t.Fatalf("staged file = %q, %v", got, err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("the client's original should be left in place: %v", err)
	}
	if _, err := os.Stat(dst + ".adopting"); !os.IsNotExist(err) {
		t.Errorf("temporary copy left behind: %v", err)
	}
}

// A writable client folder is still a plain rename.
func TestAdoptRenamesWhenItCan(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "complete", "01 Song.flac")
	stage := filepath.Join(root, "staging")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst, copied, err := acqAdopt(src, stage)
	if err != nil || copied {
		t.Fatalf("adopt = %s copied=%v err=%v, want a rename", dst, copied, err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source should have moved")
	}
}
