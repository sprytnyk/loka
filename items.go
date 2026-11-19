package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxOfferItems caps the names shown for one incoming transfer.
const maxOfferItems = 100

// entry is one file or directory in a transfer, in the order it is sent.
type entry struct {
	src   string // path on this device
	name  string // slash-separated path in the transfer
	dir   bool
	size  int64
	mtime time.Time
}

// collect lists what to send for the chosen paths, with the top-level names
// and the total size of the files. Each path becomes a top-level item, and
// items that share a name get numbered names. A chosen path must be a file
// or a directory; inside directories, symbolic links and other special
// files are left out.
func collect(paths []string) (entries []entry, items []string, size int64, err error) {
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

		err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.Type().IsRegular() && !d.IsDir() {
				return nil // symbolic links, sockets, devices
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(p, path)
			if err != nil {
				return err
			}
			e := entry{src: path, name: name, dir: d.IsDir(), mtime: info.ModTime()}
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
		if err != nil {
			return nil, nil, 0, fmt.Errorf("Cannot send %s: %w", p, err)
		}
	}
	return entries, items, size, nil
}

// numbered returns name with " (i)" before its extension: "a.txt" becomes
// "a (1).txt". Names that start with a dot keep it as part of the stem.
func numbered(name string, i int) string {
	if i == 0 {
		return name
	}
	stem, ext := name, ""
	if dot := strings.LastIndex(name, "."); dot > 0 && dot < len(name)-1 {
		stem, ext = name[:dot], name[dot:]
	}
	return fmt.Sprintf("%s (%d)%s", stem, i, ext)
}

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
