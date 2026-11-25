//go:build !linux && !darwin && !windows

package filemove

func NoReplace(from, to string) error {
	return ErrUnsupported
}
