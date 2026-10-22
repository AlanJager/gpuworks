package udocker

import "fmt"

// launchReserve is the least free space a workload starts with. The engine
// writes its log, caches and compiled kernels as the run user, who cannot use
// the blocks a filesystem keeps for root: with only that reserve left every
// root-side launch step succeeds and the engine then dies with no output.
const launchReserve = 1 << 30

// checkLaunchSpace refuses to start a workload on a disk the run user cannot
// write to: the container tree and the log directory each need launchReserve
// free. Free space that cannot be measured is not a reason to refuse.
func (d *Driver) checkLaunchSpace() error {
	free := d.freeSpace
	if free == nil {
		free = freeBytes
	}
	for _, dir := range []string{d.udockerDir, d.stateDir} {
		if dir == "" {
			continue
		}
		if avail := free(dir); avail >= 0 && avail < launchReserve {
			return fmt.Errorf("not enough disk space to start the container: %s free under %s, at least %s needed; free up space on the node",
				gib(avail), dir, gib(launchReserve))
		}
	}
	return nil
}

func gib(n int64) string { return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30)) }
