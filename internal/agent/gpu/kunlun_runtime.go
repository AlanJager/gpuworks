package gpu

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// XDR is the only transport device owned by this backend. Verbs devices,
// rails and resource limits belong to the server's network resolution.
// Reuse the native device inspector so tests and production both use Lstat.
func (p *KunlunProber) verifyKunlunXDR() error {
	info, err := p.inspect("/dev/xdrdrv")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
            return nil
        }
        return fmt.Errorf("Kunlun multi-node runtime XDR inspection: %w", err)
	}
	mode := info.Mode()
	if mode&os.ModeSymlink != 0 || mode&os.ModeDevice == 0 || mode&os.ModeCharDevice == 0 {
		return fmt.Errorf("Kunlun multi-node runtime requires XDR: /dev/xdrdrv is not a real character device")
	}
	return nil
}

// Check the effective container resources, not the identity of the selected
// network. Docker arguments alone cannot establish how that network was chosen.
func resolvedKunlunRDMAArgs(args []string) error {
	hasRDMA, hasMemlock, hasLock := false, false, false
	for i := 0; i < len(args); i++ {
		key, value, eq := strings.Cut(args[i], "=")
		if !eq && (key == "--device" || key == "--ulimit" || key == "--cap-add") && i+1 < len(args) {
			i++
			value = args[i]
		}
		if key == "--device" && (value == "/dev/infiniband" || value == "/dev/infiniband:/dev/infiniband" || value == "/dev/infiniband:/dev/infiniband:rwm") {
			hasRDMA = true
		}
		if key == "--cap-add" && strings.EqualFold(value, "IPC_LOCK") {
			hasLock = true
		}
		if key == "--ulimit" && strings.HasPrefix(value, "memlock=") {
			hasMemlock = true
			if value != "memlock=-1" && value != "memlock=-1:-1" {
				return fmt.Errorf("Kunlun host RDMA requires unlimited memlock; resolved limit is finite")
			}
		}
	}
	if !hasRDMA {
		return fmt.Errorf("Kunlun multi-node runtime requires resolved host RDMA device access")
	}
	if !hasLock {
		return fmt.Errorf("Kunlun host RDMA requires IPC_LOCK capability")
	}
	if !hasMemlock {
		return fmt.Errorf("Kunlun host RDMA requires an explicit unlimited memlock limit")
	}
	return nil
}

var _ TopologyAwareInjector = (*KunlunProber)(nil)

func (p *KunlunProber) RuntimeInjectionForLaunch(ctx context.Context, indexes []int, driver string, nNodes int, dockerArgs []string) RuntimeInjection {
	if len(indexes) == 0 {
		return RuntimeInjection{}
	}
	inj := p.runtimeInjectionForDriver(ctx, indexes, driver)
	if inj.UnsupportedReason != "" {
		return inj
	}
	if nNodes > 1 {
		if err := resolvedKunlunRDMAArgs(dockerArgs); err != nil {
			return RuntimeInjection{UnsupportedReason: err.Error()}
		}
		if err := p.verifyKunlunXDR(); err != nil {
			return RuntimeInjection{UnsupportedReason: err.Error()}
		}
		inj.DockerArgs = append(inj.DockerArgs, "--device", "/dev/xdrdrv")
	}
	if err := ctx.Err(); err != nil {
		return RuntimeInjection{UnsupportedReason: err.Error()}
	}
	return inj
}
