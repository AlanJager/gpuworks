//go:build unix

package serverconfig

import (
	"os"
	"syscall"
)

func openOutboundConfiguration(path string) (*os.File, error) {
	// Check the type on this descriptor after opening. A pathname pre-check
	// would still permit a replacement FIFO to block waiting for a writer.
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
