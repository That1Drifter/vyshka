package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// stage writes a release-like folder whose filesystem metadata is what a
// Windows checkout or an old clone would leave: modes and times that must
// not reach the archive.
func stage(t *testing.T, mtime time.Time, mode os.FileMode) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "vyshka-hub_0.1.0_linux_amd64")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"vyshka-hub": "ELF", "README.md": "read me\n", "LICENSE": "apache\n", "sub/hub.env.example": "#\n",
	} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestArchivesDependOnContentAlone(t *testing.T) {
	when := time.Unix(1758067200, 0).UTC()
	a := stage(t, time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC), 0o600)
	b := stage(t, time.Date(2030, 6, 6, 6, 6, 6, 0, time.UTC), 0o777)
	execs := map[string]bool{"vyshka-hub": true}

	for _, format := range []string{"tar.gz", "zip"} {
		var first, second bytes.Buffer
		for i, dir := range []string{a, b} {
			members, top, err := collect(dir, execs)
			if err != nil {
				t.Fatal(err)
			}
			out := &first
			if i == 1 {
				out = &second
			}
			if format == "zip" {
				err = writeZip(out, members, when)
			} else {
				err = writeTarGz(out, top, members, when)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		if !bytes.Equal(first.Bytes(), second.Bytes()) {
			t.Errorf("%s: two stagings with different modes and times gave different archives", format)
		}
		if format == "zip" {
			checkZip(t, first.Bytes(), when)
		} else {
			checkTarGz(t, first.Bytes(), when)
		}
	}
}

func checkTarGz(t *testing.T, data []byte, when time.Time) {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if !gz.Header.ModTime.IsZero() || gz.Header.Name != "" {
		t.Errorf("gzip header carries host data: %+v", gz.Header)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		if !h.ModTime.Equal(when) || h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" {
			t.Errorf("%s: time %v uid %d gid %d uname %q gname %q", h.Name, h.ModTime, h.Uid, h.Gid, h.Uname, h.Gname)
		}
		want := int64(0o644)
		switch {
		case h.Typeflag == tar.TypeDir:
			want = 0o755
		case filepath.Base(h.Name) == "vyshka-hub":
			want = 0o755
		}
		if h.Mode != want {
			t.Errorf("%s: mode %o, want %o", h.Name, h.Mode, want)
		}
	}
	wantNames := []string{
		"vyshka-hub_0.1.0_linux_amd64/",
		"vyshka-hub_0.1.0_linux_amd64/LICENSE",
		"vyshka-hub_0.1.0_linux_amd64/README.md",
		"vyshka-hub_0.1.0_linux_amd64/sub/",
		"vyshka-hub_0.1.0_linux_amd64/sub/hub.env.example",
		"vyshka-hub_0.1.0_linux_amd64/vyshka-hub",
	}
	if len(names) != len(wantNames) {
		t.Fatalf("members %v, want %v", names, wantNames)
	}
	for i := range names {
		if names[i] != wantNames[i] {
			t.Errorf("member %d = %q, want %q", i, names[i], wantNames[i])
		}
	}
}

func checkZip(t *testing.T, data []byte, when time.Time) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != 4 {
		t.Fatalf("%d members, want 4", len(zr.File))
	}
	for _, f := range zr.File {
		if !f.Modified.Equal(when) {
			t.Errorf("%s: modified %v, want %v", f.Name, f.Modified, when)
		}
		want := os.FileMode(0o644)
		if filepath.Base(f.Name) == "vyshka-hub" {
			want = 0o755
		}
		if f.Mode() != want {
			t.Errorf("%s: mode %o, want %o", f.Name, f.Mode(), want)
		}
	}
}

func TestRunRefusesBadArguments(t *testing.T) {
	dir := stage(t, time.Now(), 0o644)
	out := filepath.Join(t.TempDir(), "x.7z")
	if err := run([]string{"-out", out, "-epoch", "1", dir}); err == nil {
		t.Error("an unknown suffix was accepted")
	}
	if err := run([]string{"-out", filepath.Join(t.TempDir(), "x.zip"), dir}); err == nil {
		t.Error("a missing epoch was accepted")
	}
	if err := run([]string{"-out", filepath.Join(t.TempDir(), "x.zip"), "-epoch", "1"}); err == nil {
		t.Error("a missing directory was accepted")
	}
}
