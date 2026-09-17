package pbo

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	files := []File{
		{Name: `config.cpp`, Data: []byte("class CfgPatches {};\n"), ModTime: 1_700_000_000},
		{Name: `scripts\3_Game\Vyshka\VyshkaJson.c`, Data: []byte("class VyshkaJson {}\n"), ModTime: 1_700_000_001},
		{Name: `empty.txt`, Data: nil, ModTime: 0},
	}
	var buf bytes.Buffer
	if err := Write(&buf, "Vyshka", files); err != nil {
		t.Fatal(err)
	}

	archive, err := Read(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if archive.Extensions["prefix"] != "Vyshka" {
		t.Fatalf("prefix = %q, want Vyshka", archive.Extensions["prefix"])
	}
	if len(archive.Files) != len(files) {
		t.Fatalf("got %d files, want %d", len(archive.Files), len(files))
	}
	for i, want := range files {
		got := archive.Files[i]
		if got.Name != want.Name || !bytes.Equal(got.Data, want.Data) || got.ModTime != want.ModTime {
			t.Errorf("file %d = %+v, want %+v", i, got, want)
		}
	}
}

// The layout the game expects: the version entry first with "Vers" as its
// packing method, the extension strings, one entry per file, the terminating
// empty entry, the bodies, then a zero byte and the SHA-1 of everything
// before it.
func TestWireLayout(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, "P", []File{{Name: "a", Data: []byte("xy"), ModTime: 7}}); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()

	want := []byte{0}
	want = binary.LittleEndian.AppendUint32(want, versionMethod)
	want = append(want, make([]byte, 16)...)
	want = append(want, "prefix\x00P\x00\x00"...)
	want = append(want, "a\x00"...)
	for _, v := range []uint32{0, 2, 0, 7, 2} {
		want = binary.LittleEndian.AppendUint32(want, v)
	}
	want = append(want, 0)
	want = append(want, make([]byte, 20)...)
	want = append(want, "xy"...)
	sum := sha1.Sum(want)
	want = append(want, 0)
	want = append(want, sum[:]...)

	if !bytes.Equal(raw, want) {
		t.Fatalf("layout mismatch\n got % x\nwant % x", raw, want)
	}
}

func TestReadRejectsTamperedChecksum(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, "P", []File{{Name: "a", Data: []byte("xy")}}); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	raw[len(raw)-1] ^= 0xFF
	if _, err := Read(bytes.NewReader(raw)); err == nil {
		t.Fatal("a tampered checksum was accepted")
	}
}

func TestPackWalksSortedWithBackslashes(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(rel, content string) {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("scripts/5_Mission/b.c", "b")
	mustWrite("config.cpp", "c")
	mustWrite("scripts/3_Game/a.c", "a")

	var buf bytes.Buffer
	if err := Pack(&buf, dir, "Vyshka", Options{}); err != nil {
		t.Fatal(err)
	}
	archive, err := Read(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	wantNames := []string{`config.cpp`, `scripts\3_Game\a.c`, `scripts\5_Mission\b.c`}
	if len(archive.Files) != len(wantNames) {
		t.Fatalf("got %d files, want %d", len(archive.Files), len(wantNames))
	}
	for i, name := range wantNames {
		if archive.Files[i].Name != name {
			t.Errorf("file %d name = %q, want %q", i, archive.Files[i].Name, name)
		}
	}
}

// The archive must depend on the tree's bytes and the options alone: the
// same tree packed from a checkout with CRLF line endings, from one with
// different file modification times, or on another day, is the same bytes.
func TestPackIsReproducible(t *testing.T) {
	write := func(dir, rel, content string) {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lf, crlf := t.TempDir(), t.TempDir()
	write(lf, "config.cpp", "class A\n{\n};\n")
	write(lf, "scripts/3_Game/a.c", "void a()\n{\n}\n")
	write(crlf, "config.cpp", "class A\r\n{\r\n};\r\n")
	write(crlf, "scripts/3_Game/a.c", "void a()\r\n{\r\n}\r\n")
	// Different modification times on the second tree, so a packer that
	// still read them would show up.
	old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, rel := range []string{"config.cpp", "scripts/3_Game/a.c"} {
		if err := os.Chtimes(filepath.Join(crlf, filepath.FromSlash(rel)), old, old); err != nil {
			t.Fatal(err)
		}
	}

	opts := Options{ModTime: 1758067200, NormalizeLineEndings: true}
	var a, b bytes.Buffer
	if err := Pack(&a, lf, "Vyshka", opts); err != nil {
		t.Fatal(err)
	}
	if err := Pack(&b, crlf, "Vyshka", opts); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatalf("the LF and CRLF trees packed to different archives (%d and %d bytes)", a.Len(), b.Len())
	}
	archive, err := Read(bytes.NewReader(a.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range archive.Files {
		if f.ModTime != opts.ModTime {
			t.Errorf("%s: modTime = %d, want %d", f.Name, f.ModTime, opts.ModTime)
		}
		if bytes.Contains(f.Data, []byte("\r")) {
			t.Errorf("%s: carriage return survived normalization", f.Name)
		}
	}

	// Without normalization the two trees are different bytes and must pack
	// differently: the option does the work, not the packer by accident.
	var c bytes.Buffer
	if err := Pack(&c, crlf, "Vyshka", Options{ModTime: opts.ModTime}); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a.Bytes(), c.Bytes()) {
		t.Fatal("CRLF tree packed without normalization equals the LF archive")
	}
}
