//go:build !linux && !darwin && !windows

package transfer

func renameNoReplace(from, to string) error {
	return renameIfFree(from, to)
}
