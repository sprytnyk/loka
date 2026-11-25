package transfer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// PublishMove records the destination of one top-level rename before it is
// attempted. The next move or the completion receipt replaces the record.
type PublishMove struct {
	Index  int
	Target string
}

func PublishSaved(ctx context.Context, staging, destination string, names []string, move PublishMove, save func(PublishMove) error) error {
	if move.Index < 0 || move.Index > len(names) || move.Target != "" &&
		(move.Index == len(names) || !simpleName(move.Target)) {
		return errors.New("Invalid saved file move")
	}
	for _, name := range names {
		if !simpleName(name) {
			return errors.New("Invalid saved file name")
		}
	}
	for i := move.Index; i < len(names); i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		from := filepath.Join(staging, names[i])
		target := ""
		if i == move.Index {
			target = move.Target
		}
		if _, err := os.Lstat(from); err != nil {
			if !errors.Is(err, fs.ErrNotExist) || target == "" {
				return err
			}
			// A restart can land after the rename and before the next
			// checkpoint. The staging directory is private to this upload.
			if _, err := os.Lstat(filepath.Join(destination, target)); err != nil {
				return fmt.Errorf("could not find the file being saved: %w", err)
			}
			continue
		}
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			if target == "" {
				to, err := freePath(destination, names[i])
				if err != nil {
					return err
				}
				target = filepath.Base(to)
				if err := save(PublishMove{Index: i, Target: target}); err != nil {
					return err
				}
			}
			err := renameNoReplace(from, filepath.Join(destination, target))
			if errors.Is(err, fs.ErrExist) {
				target = ""
				continue
			}
			if err != nil {
				return err
			}
			break
		}
	}
	return nil
}

func simpleName(name string) bool {
	return filepath.IsLocal(name) && filepath.Base(name) == name && name != "."
}
