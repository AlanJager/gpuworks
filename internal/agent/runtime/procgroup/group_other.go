//go:build !linux

package procgroup

// Without Linux procfs, keep waiting for signal 0 to report that the group is gone.
func groupStillWorking(int) bool { return true }
