package main

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

// modVersion reads the version the plugin reports in its manifest, so the
// mod.cpp and the release tag are taken from the one place it is stated.
func TestModVersionReadsThePluginConstant(t *testing.T) {
	version, err := modVersion(filepath.Join("..", "..", "mod"))
	if err != nil {
		t.Fatal(err)
	}
	if version == "" {
		t.Fatal("empty version")
	}
	src := t.TempDir()
	write := func(content string) {
		path := filepath.Join(src, filepath.FromSlash(versionFile))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("class VyshkaPlugin\n{\n\tstatic const string PLUGIN_VERSION = \"1.2.3-rc.1\";\n}\n")
	if got, err := modVersion(src); err != nil || got != "1.2.3-rc.1" {
		t.Errorf("modVersion = %q, %v; want 1.2.3-rc.1", got, err)
	}
	write("class VyshkaPlugin\n{\n\tstatic const string PLUGIN_VERSION = \"seven\";\n}\n")
	if _, err := modVersion(src); err == nil {
		t.Error("a version that is not SemVer was accepted")
	}
	write("class VyshkaPlugin\n{\n\tstatic const string PLUGIN_VERSION = \"1.2.3+build.4\";\n}\n")
	if _, err := modVersion(src); err == nil {
		t.Error("build metadata was accepted; it cannot appear in a release tag")
	}
	// A mention in a comment is not the declaration, on the same line or
	// on its own, and two declarations are a contradiction, not a choice.
	write("class VyshkaPlugin\n{\n\t/* static const string PLUGIN_VERSION = \"0.6.0\"; */ static const string PLUGIN_VERSION = \"0.7.0\";\n}\n")
	if got, err := modVersion(src); err != nil || got != "0.7.0" {
		t.Errorf("modVersion beside a same-line block comment = %q, %v; want 0.7.0", got, err)
	}
	write("class VyshkaPlugin\n{\n\t// static const string PLUGIN_VERSION = \"0.6.0\";\n\tstatic const string PLUGIN_VERSION = \"0.7.0\";\n}\n")
	if got, err := modVersion(src); err != nil || got != "0.7.0" {
		t.Errorf("modVersion beside a commented-out line = %q, %v; want 0.7.0", got, err)
	}
	write("class VyshkaPlugin\n{\n\tstatic const string PLUGIN_VERSION = \"0.6.0\";\n\tstatic const string PLUGIN_VERSION = \"0.7.0\";\n}\n")
	if _, err := modVersion(src); err == nil {
		t.Error("two declarations were accepted")
	}
	// A block comment spanning lines hides its declaration, a trailing
	// line comment does not hide the real one, and a comment in the
	// middle of the file is the engine's business, not the parser's.
	write("class VyshkaPlugin\n{\n\t/*\n\tstatic const string PLUGIN_VERSION = \"0.6.0\";\n\t*/\n\tstatic const string PLUGIN_VERSION = \"0.7.0\"; // current\n}\n")
	if got, err := modVersion(src); err != nil || got != "0.7.0" {
		t.Errorf("modVersion with a block comment above and a line comment beside = %q, %v; want 0.7.0", got, err)
	}
	write("class VyshkaPlugin\n{\n\t/* old\n\tstatic const string PLUGIN_VERSION = \"0.6.0\";\n\t*/\n}\n")
	if _, err := modVersion(src); err == nil {
		t.Error("a declaration inside a block comment was accepted")
	}
	// Comment delimiters inside string literals are text: the scanner must
	// not take a "/*" in one string and a "*/" in another for a comment
	// around the declaration, nor a "//" in a URL for a line comment, and
	// an escaped quote does not end a string early.
	write("class VyshkaPlugin\n{\n\tstatic const string OPEN_MARKER = \"/*\";\n\tstatic const string PLUGIN_VERSION = \"0.7.0\";\n\tstatic const string CLOSE_MARKER = \"*/\";\n}\n")
	if got, err := modVersion(src); err != nil || got != "0.7.0" {
		t.Errorf("modVersion between strings holding comment delimiters = %q, %v; want 0.7.0", got, err)
	}
	write("class VyshkaPlugin\n{\n\tstatic const string HUB = \"http://hub.example\"; static const string PLUGIN_VERSION = \"0.6.0\";\n\tstatic const string QUOTE = \"a\\\"/*\";\n\tstatic const string PLUGIN_VERSION = \"0.7.0\";\n}\n")
	if got, err := modVersion(src); err != nil || got != "0.7.0" {
		t.Errorf("modVersion past a URL and an escaped quote = %q, %v; want 0.7.0", got, err)
	}
	// Prerelease identifiers follow SemVer 2.0.0: a numeric one has no
	// leading zero, and an empty one is not an identifier.
	for _, bad := range []string{"1.2.3-01", "1.2.3-rc.01", "1.2.3-", "1.2.3-rc..1", "01.2.3"} {
		write("class VyshkaPlugin\n{\n\tstatic const string PLUGIN_VERSION = \"" + bad + "\";\n}\n")
		if got, err := modVersion(src); err == nil {
			t.Errorf("%s was accepted as %q", bad, got)
		}
	}
	for _, good := range []string{"1.2.3-rc.1", "1.2.3-0", "1.2.3-alpha-1.0.x", "10.0.0"} {
		write("class VyshkaPlugin\n{\n\tstatic const string PLUGIN_VERSION = \"" + good + "\";\n}\n")
		if got, err := modVersion(src); err != nil || got != good {
			t.Errorf("%s = %q, %v", good, got, err)
		}
	}
}

// Two builds of the same source, in different output directories and from
// trees with different line endings, must write the same PBO: the release
// archive is checked against a rebuild from the tagged commit.
func TestBuildIsReproducible(t *testing.T) {
	t.Setenv("SOURCE_DATE_EPOCH", "1758067200")
	source := filepath.Join("..", "..", "mod")

	// A copy of the mod tree with CRLF line endings, whatever the checkout
	// has, so the comparison exercises the normalization on a real tree.
	crlf := t.TempDir()
	err := filepath.WalkDir(source, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(source, path)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
		data = bytes.ReplaceAll(data, []byte("\n"), []byte("\r\n"))
		target := filepath.Join(crlf, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}

	digest := func(src string) [32]byte {
		out := t.TempDir()
		if err := runBuild([]string{"-src", src, "-out", out}); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(out, modDir, "addons", pboName))
		if err != nil {
			t.Fatal(err)
		}
		return sha256.Sum256(data)
	}
	if a, b := digest(source), digest(crlf); a != b {
		t.Fatalf("the checkout and its CRLF copy built different archives: %x and %x", a, b)
	}

	out := t.TempDir()
	if err := runBuild([]string{"-src", source, "-out", out}); err != nil {
		t.Fatal(err)
	}
	modCppBytes, err := os.ReadFile(filepath.Join(out, modDir, "mod.cpp"))
	if err != nil {
		t.Fatal(err)
	}
	version, _ := modVersion(source)
	if !bytes.Contains(modCppBytes, []byte("version = \""+version+"\";")) {
		t.Errorf("mod.cpp does not carry the plugin version %s:\n%s", version, modCppBytes)
	}
}
