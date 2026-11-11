//go:build unix && !solaris && !illumos && !aix

package relay

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive lock on path+".lock", waiting for it, and
// returns the function that lets it go. The relay and `sailnode pair` /
// `sailnode unpair` are separate processes editing the same files; the lock
// keeps one read-modify-write from undoing another's.
func lockFile(path string) (func(), error) {
	// Read-only and world-readable (it is empty): whichever user creates it,
	// the relay's and the operator's processes can both open it to lock.
	f, err := os.OpenFile(path+".lock", os.O_RDONLY|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// keepOwner gives a replacement file the owner of the file it replaces, as
// writing in place did, so a file `sailnode` run as root rewrites stays
// readable by a relay running as another user. Best effort: only root can.
func keepOwner(f *os.File, path string) {
	if st, err := os.Stat(path); err == nil {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			f.Chown(int(sys.Uid), int(sys.Gid))
		}
	}
}
