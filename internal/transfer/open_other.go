//go:build !unix && !windows

package transfer

import "os"

const blocksWriters = false

// openForReading opens path for reading.
func openForReading(path string) (*os.File, error) { return os.Open(path) }
