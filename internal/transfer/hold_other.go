//go:build !unix

package transfer

import "math"

const idsCanBeReused = false

var openFileLimit = func() int { return math.MaxInt32 }
