package testenv

import (
	"io"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// Injection points for the runtime fake. They are split out of fake_runtime.go
// so the fake's actual behaviour - what Run, Stop, Remove and the image paths
// do - stays readable next to the state they operate on.
//
// Each On* hook can decline to handle a call (its last return value), which is
// what keeps the fake honest: a test overrides the one case it is about and
// everything else keeps the default behaviour, including the default failures.

func (d *FakeDriver) OnRun(h RunHook) { d.mu.Lock(); d.OnRunHook = h; d.mu.Unlock() }
func (d *FakeDriver) AfterRun(h func(runtime.WorkloadSpec)) {
	d.mu.Lock()
	d.AfterRunHook = h
	d.mu.Unlock()
}
func (d *FakeDriver) OnStop(h func(string) (error, bool)) {
	d.mu.Lock()
	d.OnStopHook = h
	d.mu.Unlock()
}
func (d *FakeDriver) AfterStop(h func(string)) { d.mu.Lock(); d.AfterStopHook = h; d.mu.Unlock() }
func (d *FakeDriver) OnRemove(h func(string) (error, bool)) {
	d.mu.Lock()
	d.OnRemoveHook = h
	d.mu.Unlock()
}
func (d *FakeDriver) AfterRemove(h func(string)) { d.mu.Lock(); d.AfterRemoveHook = h; d.mu.Unlock() }
func (d *FakeDriver) OnStatus(h func(string) (runtime.Status, error, bool)) {
	d.mu.Lock()
	d.OnStatusHook = h
	d.mu.Unlock()
}
func (d *FakeDriver) AfterStatus(h func(string, runtime.Status)) {
	d.mu.Lock()
	d.AfterStatusHook = h
	d.mu.Unlock()
}
func (d *FakeDriver) OnPullImage(h func(string, func(string)) (error, bool)) {
	d.mu.Lock()
	d.OnPullImageHook = h
	d.mu.Unlock()
}
func (d *FakeDriver) AfterPullImage(h func(string)) {
	d.mu.Lock()
	d.AfterPullImageHook = h
	d.mu.Unlock()
}
func (d *FakeDriver) OnImageExists(h func(string) (bool, error, bool)) {
	d.mu.Lock()
	d.OnImageExistsHook = h
	d.mu.Unlock()
}
func (d *FakeDriver) AfterImageExists(h func(string, bool)) {
	d.mu.Lock()
	d.AfterImageExistsHook = h
	d.mu.Unlock()
}
func (d *FakeDriver) OnPrepare(h func(runtime.WorkloadSpec) (error, bool)) {
	d.mu.Lock()
	d.OnPrepareHook = h
	d.mu.Unlock()
}
func (d *FakeDriver) AfterPrepare(h func(runtime.WorkloadSpec)) {
	d.mu.Lock()
	d.AfterPrepareHook = h
	d.mu.Unlock()
}
func (d *FakeDriver) OnLogs(h func(string, bool, int) (io.ReadCloser, error, bool)) {
	d.mu.Lock()
	d.OnLogsHook = h
	d.mu.Unlock()
}
func (d *FakeDriver) AfterLogs(h func(string, bool, int)) {
	d.mu.Lock()
	d.AfterLogsHook = h
	d.mu.Unlock()
}
func (d *FakeDriver) OnExec(h func(string, []string) ([]byte, error, bool)) {
	d.mu.Lock()
	d.OnExecHook = h
	d.mu.Unlock()
}
func (d *FakeDriver) AfterExec(h func(string, []string)) {
	d.mu.Lock()
	d.AfterExecHook = h
	d.mu.Unlock()
}
