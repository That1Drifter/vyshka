// Package pbo writes and reads the PBO archive format DayZ loads addons from.
//
// A PBO is a flat archive: a header of fixed-size entries (one per file, plus
// a leading version entry carrying key/value extensions such as the prefix
// and a terminating empty entry), the file bodies concatenated in header
// order, and a trailing SHA-1 over everything before it. This package writes
// uncompressed entries only, which is all a script mod needs, and reads
// anything it wrote so the writer can be tested against itself and against
// archives the game ships.
package pbo

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// versionMethod marks the leading header entry that carries the extensions.
const versionMethod = 0x56657273 // "Vers" read as a little-endian uint32

// File is one archive member. Name uses backslashes, relative to the prefix.
type File struct {
	Name    string
	Data    []byte
	ModTime uint32
}

// Archive is a parsed PBO.
type Archive struct {
	Extensions map[string]string
	Files      []File
}

// Pack walks srcDir and writes every regular file under it into a PBO with
// the given prefix. Paths are sorted so the output is deterministic for a
// given tree; modification times come from the files.
func Pack(w io.Writer, srcDir, prefix string) error {
	var files []File
	err := filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files = append(files, File{
			Name:    strings.ReplaceAll(filepath.ToSlash(rel), "/", `\`),
			Data:    data,
			ModTime: uint32(info.ModTime().Unix()),
		})
		return nil
	})
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("pbo: no files under %s", srcDir)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return Write(w, prefix, files)
}

// Write emits a PBO with one extension, the prefix, and the given files in
// the given order.
func Write(w io.Writer, prefix string, files []File) error {
	var body bytes.Buffer
	writeEntry := func(name string, method, original, reserved, timestamp, size uint32) {
		body.WriteString(name)
		body.WriteByte(0)
		for _, v := range []uint32{method, original, reserved, timestamp, size} {
			_ = binary.Write(&body, binary.LittleEndian, v)
		}
	}

	writeEntry("", versionMethod, 0, 0, 0, 0)
	body.WriteString("prefix")
	body.WriteByte(0)
	body.WriteString(prefix)
	body.WriteByte(0)
	body.WriteByte(0)

	for _, f := range files {
		if f.Name == "" {
			return errors.New("pbo: a file has an empty name")
		}
		if strings.ContainsRune(f.Name, 0) {
			return fmt.Errorf("pbo: file name %q contains a NUL", f.Name)
		}
		size := uint32(len(f.Data))
		writeEntry(f.Name, 0, size, 0, f.ModTime, size)
	}
	writeEntry("", 0, 0, 0, 0, 0)

	for _, f := range files {
		body.Write(f.Data)
	}

	sum := sha1.Sum(body.Bytes())
	body.WriteByte(0)
	body.Write(sum[:])
	_, err := w.Write(body.Bytes())
	return err
}

// Read parses a PBO. It verifies the trailing SHA-1 and refuses compressed
// entries, which this package never writes.
func Read(r io.Reader) (*Archive, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if len(raw) < 21 {
		return nil, errors.New("pbo: too short")
	}
	payload, trailer := raw[:len(raw)-21], raw[len(raw)-21:]
	if trailer[0] != 0 {
		return nil, errors.New("pbo: trailer does not start with a zero byte")
	}
	if sum := sha1.Sum(payload); !bytes.Equal(sum[:], trailer[1:]) {
		return nil, errors.New("pbo: checksum mismatch")
	}

	br := bufio.NewReader(bytes.NewReader(payload))
	readString := func() (string, error) {
		s, err := br.ReadString(0)
		if err != nil {
			return "", err
		}
		return s[:len(s)-1], nil
	}
	readEntry := func() (name string, fields [5]uint32, err error) {
		if name, err = readString(); err != nil {
			return
		}
		for i := range fields {
			if err = binary.Read(br, binary.LittleEndian, &fields[i]); err != nil {
				return
			}
		}
		return
	}

	archive := &Archive{Extensions: map[string]string{}}
	type pending struct {
		name          string
		size, modTime uint32
	}
	var entries []pending
	for {
		name, fields, err := readEntry()
		if err != nil {
			return nil, fmt.Errorf("pbo: header: %w", err)
		}
		if name == "" && fields[0] == versionMethod {
			for {
				key, err := readString()
				if err != nil {
					return nil, err
				}
				if key == "" {
					break
				}
				value, err := readString()
				if err != nil {
					return nil, err
				}
				archive.Extensions[key] = value
			}
			continue
		}
		if name == "" {
			break
		}
		if fields[0] != 0 {
			return nil, fmt.Errorf("pbo: entry %q uses packing method %#x, which this reader does not support", name, fields[0])
		}
		entries = append(entries, pending{name: name, size: fields[4], modTime: fields[3]})
	}
	for _, e := range entries {
		data := make([]byte, e.size)
		if _, err := io.ReadFull(br, data); err != nil {
			return nil, fmt.Errorf("pbo: body of %q: %w", e.name, err)
		}
		archive.Files = append(archive.Files, File{Name: e.name, Data: data, ModTime: e.modTime})
	}
	return archive, nil
}
