//go:build !linux && !darwin && !windows

package main

func renameNoReplace(from, to string) error {
	return renameIfFree(from, to)
}
