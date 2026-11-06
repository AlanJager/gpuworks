//go:build !unix

package serverconfig

import "os"

func openOutboundConfiguration(path string) (*os.File, error) {
	return os.Open(path)
}
