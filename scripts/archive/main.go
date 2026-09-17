// Command archive writes a release archive whose bytes depend on the staged
// files alone, on any host: one implementation for the tar.gz and the zip,
// members sorted, owned by nobody, dated by the given epoch in UTC, with a
// mode that comes from the flags rather than from the filesystem (Windows
// has no executable bit to inherit, and a zip tool's idea of local time
// differs between hosts). scripts/release-hub.sh calls it once per archive.
//
//	go run ./scripts/archive -out dist/x.tar.gz -epoch 1758067200 -exec vyshka-hub dist/x
//	go run ./scripts/archive -out dist/x.zip -epoch 1758067200 -exec vyshka-hub.exe dist/x
//
// The directory's base name becomes the top-level directory inside the
// archive, so the archive unpacks into a folder of that name.
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	modeFile = 0o644
	modeExec = 0o755
	modeDir  = 0o755
)

type member struct {
	// name is the path inside the archive, forward slashes, no leading
	// directory beyond the top-level folder.
	name string
	path string
	size int64
	exec bool
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "archive:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("archive", flag.ContinueOnError)
	out := fs.String("out", "", "archive to write; .tar.gz or .zip decides the format")
	epoch := fs.Int64("epoch", -1, "timestamp of every member, seconds since the Unix epoch")
	execList := fs.String("exec", "", "comma-separated base names of members stored executable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || *out == "" || *epoch < 0 {
		return fmt.Errorf("usage: archive -out <file.tar.gz|file.zip> -epoch <seconds> [-exec name,...] <directory>")
	}
	execs := map[string]bool{}
	for _, name := range strings.Split(*execList, ",") {
		if name != "" {
			execs[name] = true
		}
	}
	members, top, err := collect(fs.Arg(0), execs)
	if err != nil {
		return err
	}
	file, err := os.Create(*out)
	if err != nil {
		return err
	}
	when := time.Unix(*epoch, 0).UTC()
	switch {
	case strings.HasSuffix(*out, ".tar.gz"):
		err = writeTarGz(file, top, members, when)
	case strings.HasSuffix(*out, ".zip"):
		err = writeZip(file, members, when)
	default:
		err = fmt.Errorf("%s: the format is taken from the suffix, .tar.gz or .zip", *out)
	}
	if err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// collect walks dir and returns its regular files, sorted by archive name,
// with the directory's base name as the top-level folder.
func collect(dir string, execs map[string]bool) ([]member, string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, "", err
	}
	top := filepath.Base(abs)
	var members []member
	err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: not a regular file", p)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(abs, p)
		if err != nil {
			return err
		}
		members = append(members, member{
			name: path.Join(top, filepath.ToSlash(rel)),
			path: p,
			size: info.Size(),
			exec: execs[d.Name()],
		})
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	if len(members) == 0 {
		return nil, "", fmt.Errorf("%s: no files", dir)
	}
	sort.Slice(members, func(i, j int) bool { return members[i].name < members[j].name })
	return members, top, nil
}

func writeTarGz(w io.Writer, top string, members []member, when time.Time) error {
	gz, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return err
	}
	// The gzip header's own time stays zero and its OS byte "unknown":
	// nothing about the host reaches the bytes.
	tw := tar.NewWriter(gz)
	dirs := map[string]bool{}
	writeDir := func(name string) error {
		if dirs[name] {
			return nil
		}
		dirs[name] = true
		// PAX, not USTAR: a long prerelease name makes the top-level
		// directory longer than USTAR's name field can hold, and without
		// an inner slash Go cannot split it. PAX carries the name in an
		// extended record whose content is deterministic, and a name that
		// fits needs no record at all.
		return tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeDir, Name: name + "/", Mode: modeDir, ModTime: when, Format: tar.FormatPAX,
		})
	}
	if err := writeDir(top); err != nil {
		return err
	}
	for _, m := range members {
		// Every directory on the way, in order, so an extractor that
		// creates parents from entries gets the same modes everywhere.
		for i := len(top); i < len(m.name); i++ {
			if m.name[i] == '/' {
				if err := writeDir(m.name[:i]); err != nil {
					return err
				}
			}
		}
		mode := int64(modeFile)
		if m.exec {
			mode = modeExec
		}
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg, Name: m.name, Mode: mode, Size: m.size, ModTime: when, Format: tar.FormatPAX,
		}); err != nil {
			return err
		}
		if err := copyFile(tw, m.path); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func writeZip(w io.Writer, members []member, when time.Time) error {
	zw := zip.NewWriter(w)
	for _, m := range members {
		mode := fs.FileMode(modeFile)
		if m.exec {
			mode = modeExec
		}
		header := &zip.FileHeader{Name: m.name, Method: zip.Deflate, Modified: when}
		header.SetMode(mode)
		entry, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		if err := copyFile(entry, m.path); err != nil {
			return err
		}
	}
	return zw.Close()
}

func copyFile(w io.Writer, p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}
