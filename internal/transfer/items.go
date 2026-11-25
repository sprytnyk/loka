package transfer

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// MaxOfferItems caps the names shown for one incoming transfer.
const MaxOfferItems = 100

// Entry is one file or directory in a transfer, in the order it is sent.
type Entry struct {
	src  string // path on this device
	name string // slash-separated path in the transfer
	dir  bool
	size int64
	info os.FileInfo // what Collect saw, so sending can tell a replacement
}

// Collect lists what to send for the chosen paths, with the top-level names
// and the total size of the files. Each path becomes a top-level item, and
// items that share a name get numbered names. A chosen path must be a file
// or a directory; inside directories, symbolic links and other special
// files are left out.
func Collect(paths []string) (entries []Entry, items []string, size int64, err error) {
	if len(paths) == 0 {
		return nil, nil, 0, errors.New("Choose one or more files or directories")
	}
	taken := map[string]bool{}
	for _, p := range paths {
		taken[filepath.Base(filepath.Clean(p))] = true
	}
	used := map[string]bool{}
	for _, p := range paths {
		p = filepath.Clean(p)
		info, err := os.Lstat(p)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("Cannot send %s: %w", p, err)
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return nil, nil, 0, fmt.Errorf("Cannot send %s: only files and directories can be sent", p)
		}
		base := filepath.Base(p)
		if base == string(filepath.Separator) || base == "." {
			return nil, nil, 0, fmt.Errorf("Cannot send %s: choose a directory inside it", p)
		}
		name := base
		if used[name] {
			for i := 1; used[name] || taken[name]; i++ {
				name = numbered(base, i)
			}
		}
		used[name] = true
		items = append(items, name)

		found, n, err := walk(p, name)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("Cannot send %s: %w", p, err)
		}
		entries = append(entries, found...)
		size += n
	}
	return entries, items, size, nil
}

// walk lists root and everything under it, named under name in the
// transfer, and adds up the file sizes. Symbolic links, sockets and
// devices are left out.
func walk(root, name string) (entries []Entry, size int64, err error) {
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() && !d.IsDir() {
			return nil
		}
		// The entry may have changed since the directory was listed, to
		// a named pipe for one, so the fresh info must agree with d.
		info, err := lstat(path)
		if err != nil {
			return err
		}
		if info.Mode().Type() != d.Type() {
			return fmt.Errorf("%s %w", path, errReplaced)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		e := Entry{src: path, name: name, dir: d.IsDir(), info: info}
		if rel != "." {
			e.name += "/" + filepath.ToSlash(rel)
		}
		if !e.dir {
			e.size = info.Size()
			size += e.size
		}
		entries = append(entries, e)
		return nil
	})
	return entries, size, err
}

// maxNameBytes is the longest file name most systems allow.
const maxNameBytes = 255

// numbered returns name with " (i)" before its extension: "a.txt" becomes
// "a (1).txt". Names that start with a dot keep it as part of the stem. The
// stem is shortened when the result would be longer than maxNameBytes.
func numbered(name string, i int) string {
	if i == 0 {
		return name
	}
	mark := fmt.Sprintf(" (%d)", i)
	stem, ext := name, ""
	if dot := strings.LastIndex(name, "."); dot > 0 && dot < len(name)-1 && len(name)-dot+len(mark) < maxNameBytes {
		stem, ext = name[:dot], name[dot:]
	}
	return cutToBytes(stem, maxNameBytes-len(mark)-len(ext)) + mark + ext
}

// cutToBytes shortens s to at most n bytes without splitting a character.
func cutToBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// lstat is os.Lstat that reads the file's identity now. On Windows,
// os.Lstat and d.Info leave the file ID to be read by the first
// os.SameFile, from whatever is at the path by then.
func lstat(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err == nil {
		os.SameFile(info, info)
	}
	return info, err
}

// sameFile reports whether now is still the file or directory that was.
// A new file can get a deleted one's identity, on Unix and on FAT drives;
// unchanged also catches that.
func sameFile(was, now os.FileInfo) bool {
	return os.SameFile(was, now) && now.Mode().Type() == was.Mode().Type()
}

// unchanged reports whether now is the file that was, with nothing about
// it changed since, as far as changeStamp can tell. A file made later in a
// deleted one's place gets a later stamp even when it gets the same ID.
func unchanged(was, now os.FileInfo) bool {
	return sameFile(was, now) && changeStamp(was) == changeStamp(now)
}

// stamp marks a version of a file: times that move when the file changes.
type stamp [2]int64

// windowsName changes a file name that Windows cannot use: it replaces
// characters Windows forbids, drops trailing spaces and dots, and puts "_"
// before reserved device names such as CON.
func windowsName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`<>:"|?*`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.TrimRight(name, " .")
	if name == "" {
		return "_"
	}
	stem, _, _ := strings.Cut(name, ".")
	if reservedNames[strings.ToUpper(strings.TrimRight(stem, " "))] {
		return "_" + name
	}
	return name
}

// reservedNames are the device names Windows reserves, with or without an
// extension.
var reservedNames = func() map[string]bool {
	names := map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true, "CONIN$": true, "CONOUT$": true}
	for _, n := range []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³"} {
		names["COM"+n] = true
		names["LPT"+n] = true
	}
	return names
}()
