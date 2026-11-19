//go:build !unix

package transfer

import (
	"math"
	"os"
)

const holdsFiles = false

var openFileLimit = func() int { return math.MaxInt32 }

// openForReading opens path for reading. Named pipes don't live at file
// paths on Windows, and openChosen's checks catch a link put in the path's
// place.
func openForReading(path string) (*os.File, error) { return os.Open(path) }
