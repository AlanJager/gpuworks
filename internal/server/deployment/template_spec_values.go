package deployment

// The selected spec's own launch values, read where a launch input is built;
// each is the zero value when the deployment has no template or the spec
// states none.

// shmBytes is the /dev/shm size the selected spec states, or zero when the
// deployment has no template or the spec states none.
func (s templateSelection) shmBytes() int64 {
	if s.Spec == nil {
		return 0
	}
	return s.Spec.ShmBytes
}

// startTimeoutSec is the instance start timeout the selected spec states, or
// zero when the deployment has no template or the spec states none.
func (s templateSelection) startTimeoutSec() int {
	if s.Spec == nil {
		return 0
	}
	return s.Spec.StartTimeoutSec
}

// memlockUnlimited reports whether the selected spec asks for unlimited locked
// memory in its container.
func (s templateSelection) memlockUnlimited() bool {
	return s.Spec != nil && s.Spec.MemlockUnlimited
}

// dockerArgs is the selected spec's own docker run options, decoded once in
// applyTemplateDefaults (DockerArgs); nil when the deployment has no template
// or the spec states none.
func (s templateSelection) dockerArgs() []string {
	return s.DockerArgs
}
