package transfer

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SourceFile records the name and version of a chosen item across runs.
type SourceFile struct {
	Path     string
	Name     string
	Mode     os.FileMode
	Size     int64
	Modified time.Time
	Changed  stamp
}

func Snapshot(entries []Entry) ([]SourceFile, error) {
	files := make([]SourceFile, 0, len(entries))
	for _, e := range entries {
		path, err := filepath.Abs(e.src)
		if err != nil {
			return nil, err
		}
		files = append(files, SourceFile{
			Path: path, Name: e.name, Mode: e.info.Mode(), Size: e.size,
			Modified: e.info.ModTime(), Changed: changeStamp(e.info),
		})
	}
	return files, nil
}

// Restore checks that the files still describe the same tar stream.
func Restore(files []SourceFile) ([]Entry, error) {
	entries := make([]Entry, 0, len(files))
	for _, f := range files {
		info, err := lstat(f.Path)
		if err != nil || info.Mode() != f.Mode ||
			!info.ModTime().Equal(f.Modified) || changeStamp(info) != f.Changed ||
			!info.IsDir() && info.Size() != f.Size {
			return nil, fmt.Errorf("%s %w", f.Path, errReplaced)
		}
		entries = append(entries, Entry{src: f.Path, name: f.Name, size: f.Size, dir: info.IsDir(), info: info})
	}
	return entries, nil
}
