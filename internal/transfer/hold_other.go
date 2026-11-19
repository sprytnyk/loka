//go:build !unix

package transfer

import "math"

const holdsFiles = false

var openFileLimit = func() int { return math.MaxInt32 }
