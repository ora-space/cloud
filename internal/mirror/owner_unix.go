//go:build unix

package mirror

import (
	"os"
	"syscall"
)

// volumeOwner reports the uid owning a path, and whether this platform has such a thing.
//
// It is a build-tagged file rather than a condition in the store because the answer is a property of
// the operating system, and the platforms without it must not silently behave as if the owner were
// zero: adopting a uid that means nothing would chown the store's files to root.
func volumeOwner(info os.FileInfo) (int, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(stat.Uid), true
}
