//go:build !unix

package relay

import "os"

// lockFile is a no-op where flock is not available. Relays run on Linux;
// this keeps the package building for the desktop clients that import it.
func lockFile(path string) (func(), error) { return func() {}, nil }

// keepOwner: file ownership is left as the platform gives it.
func keepOwner(f *os.File, path string) {}
