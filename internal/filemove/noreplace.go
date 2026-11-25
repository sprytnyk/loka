// Package filemove moves files without replacing an existing destination.
package filemove

import "errors"

// ErrUnsupported means the filesystem or platform cannot do an exclusive rename.
var ErrUnsupported = errors.New("rename without replacement is unsupported")
