//go:build !unix

package mirror

import "os"

// volumeOwner reports that this platform has no numeric file owner, so the store adopts none and
// every file it creates keeps the identity of the process that wrote it — which is the only identity
// such a platform has. The shared-volume case this exists for is a deployment fact on Unix.
func volumeOwner(os.FileInfo) (int, bool) { return 0, false }
