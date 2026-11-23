//go:build !linux

package procgroup

// Without Linux procfs, keep waiting for signal 0 to report that the group is gone.
func groupStillWorking(int) bool { return true }

// The same for one process: only signal 0 can say whether it is still there.
func processStillWorking(int) bool { return true }
