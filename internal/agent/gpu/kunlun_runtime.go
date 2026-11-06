package gpu

import (
	"context"
	"fmt"
	"os"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// XDR is the only transport device owned by this backend. Verbs devices,
// rails and resource limits belong to the server's network resolution.
// Reuse the native device inspector so tests and production both use Lstat.
func (p *KunlunProber) verifyKunlunXDR() error {
	info, err := p.inspect("/dev/xdrdrv")
	if err != nil {
		return fmt.Errorf("Kunlun multi-node runtime requires XDR: %w", err)
	}
	mode := info.Mode()
	if mode&os.ModeSymlink != 0 || mode&os.ModeDevice == 0 || mode&os.ModeCharDevice == 0 {
		return fmt.Errorf("Kunlun multi-node runtime requires XDR: /dev/xdrdrv is not a real character device")
	}
	return nil
}

// Check the effective container resources, not the identity of the selected
// network. Docker arguments alone cannot establish how that network was chosen.
func kunlunResourceError(resource, reason string) *InjectionError {
	return &InjectionError{Failure: proto.RuntimeFailure{Code: proto.ErrorAcceleratorResourceUnavailable, Vendor: accelerator.VendorKunlunxin, Resource: resource}, Reason: reason}
}

func resolvedKunlunRDMAArgs(args []string) *InjectionError {
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
				return kunlunResourceError("memlock", "Kunlun host RDMA requires unlimited memlock; resolved limit is finite")
			}
		}
	}
	if !hasRDMA {
		return kunlunResourceError("/dev/infiniband", "Kunlun multi-node runtime requires resolved host RDMA device access")
	}
	if !hasLock {
		return kunlunResourceError("IPC_LOCK", "Kunlun host RDMA requires IPC_LOCK capability")
	}
	if !hasMemlock {
		return kunlunResourceError("memlock", "Kunlun host RDMA requires an explicit unlimited memlock limit")
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
			return rejectedInjection(err.Failure, err.Error())
		}
		if err := p.verifyKunlunXDR(); err != nil {
			return rejectedInjection(proto.RuntimeFailure{Code: proto.ErrorAcceleratorResourceUnavailable, Vendor: p.Vendor(), Resource: "/dev/xdrdrv"}, err.Error())
		}
		inj.DockerArgs = append(inj.DockerArgs, "--device", "/dev/xdrdrv")
	}
	if err := ctx.Err(); err != nil {
		return RuntimeInjection{UnsupportedReason: err.Error()}
	}
	return inj
}
