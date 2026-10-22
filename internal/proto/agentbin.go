package proto

// AgentBinaryName is the file name under which the server serves the agent
// for one platform (/dl/<name>), and which an upgrade fetches or pushes.
// goos and goarch are Go's spellings, as an agent reports them in SysInfo; an
// empty goos is an agent that predates reporting it, which ran on Linux.
func AgentBinaryName(goos, goarch string) string {
	if goos == "" {
		goos = "linux"
	}
	return "gpuworks-agent-" + goos + "-" + goarch
}
