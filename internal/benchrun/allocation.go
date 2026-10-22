package benchrun

import (
	"fmt"
	"reflect"

	"gpuworks.ai/gpuworks/pkg/bench/dataset"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
)

// ValidateAllocation is the common admission and execution preflight. It
// checks the request/result arrays as well as dataset generation, without
// allocating the workload. Like dataset.ValidateAllocation, it checks
// addressability, not the machine's currently available physical memory.
func ValidateAllocation(dc dataset.Config, n int) error {
	// Requests and at least one result per session coexist with the dataset.
	perSession := uint64(reflect.TypeFor[loadgen.Request]().Size() + reflect.TypeFor[loadgen.Result]().Size())
	if n < 0 || uint64(n) > dataset.MaxAllocationBytes/perSession {
		return fmt.Errorf("request count exceeds the supported allocation size")
	}
	return dataset.ValidateAllocation(dc, n)
}
