package pbo

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
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
	if err := Pack(&buf, dir, "Vyshka"); err != nil {
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
