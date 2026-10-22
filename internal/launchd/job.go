// Package launchd is the gpuworks agent's macOS service: the one launchd job
// an install registers, its plist, and where that plist lives. The agent
// installs and removes the job (gpuworks-agent install-service, uninstall);
// the server looks for one before an SSH install replaces it. It is a leaf
// with no imports from this repository, so both sides share it.
//
// With administrator rights the job is a launch daemon of the system domain,
// running as the installing user from boot whether or not anyone is logged
// in; without them, a launch agent of that user's login session
// (docs/adr/0044-mac-engines-as-host-processes.md).
package launchd

import (
	"path"
	"strings"
)

const (
	// AgentLabel names the agent's job in whichever domain holds it.
	AgentLabel = "ai.gpuworks.agent"
	// ServerLabel names the control plane's launch daemon (gpuworks install).
	ServerLabel = "ai.gpuworks.server"
	// DaemonDir holds the launch daemon's plist.
	DaemonDir = "/Library/LaunchDaemons"
	// HomeInstallDir is where an install without administrator rights puts
	// the agent, under the user's home: /opt is not writable to them.
	HomeInstallDir = ".gpuworks"
	// StderrLog, in the state directory, takes what the agent writes before
	// its own log is open.
	StderrLog = "agent.stderr.log"
)

// DaemonPlist is the agent's launch daemon plist.
func DaemonPlist() string { return DaemonPath(AgentLabel) }

// DaemonPath is where the launch daemon labelled label keeps its plist.
func DaemonPath(label string) string { return path.Join(DaemonDir, label+".plist") }

// AgentPlist is the plist of the launch agent of the user whose home is home.
func AgentPlist(home string) string {
	return path.Join(home, "Library", "LaunchAgents", AgentLabel+".plist")
}

// Job is what the plist says about the agent it runs: the binary and the
// state directory whose agentconf file configures it (`run --state DIR`).
type Job struct {
	Binary   string
	StateDir string
	// Server is the server the agent dials, from its configuration file; the
	// plist does not hold it. Install compares it with the server a plist of
	// an earlier install names (the install script compares configuration
	// files before it replaces one).
	Server string
	// Token, ConnectMode and Listen are what a plist written before the
	// configuration file carried; ReadJob fills them, Plist never writes them.
	Token       string
	ConnectMode string
	Listen      string
	// Env is passed to the agent as is.
	Env map[string]string
	// User is the account a launch daemon runs as; empty for a launch agent,
	// which runs as the owner of its session.
	User string
}

// tokenEnv carried the node token in plists written before the configuration
// file, in the environment rather than the arguments every local user can
// read with ps.
const tokenEnv = "GPUWORKS_TOKEN"

// Plist renders the job. The flag and its value share a line, which the
// server's look for an existing agent reads --state from without a plist
// parser (provision.existingAgentCommand).
func (j Job) Plist() []byte {
	args := []string{j.Binary, "run", "--state", j.StateDir}
	env := map[string]string{}
	for k, v := range j.Env {
		env[k] = v
	}
	// AbandonProcessGroup keeps the agent's engines, process groups of their
	// own, running while launchd restarts a crashed or upgraded agent.
	return Spec{
		Label: AgentLabel, User: j.User, Args: args, Env: env,
		WorkingDir: j.StateDir, StderrPath: path.Join(j.StateDir, StderrLog),
		AbandonProcessGroup: true,
	}.Plist()
}

// ReadJob reads a plist an install wrote, including those from before the
// configuration file (server, token and the rest as arguments) and before
// the token moved out of the arguments.
func ReadJob(data []byte) (Job, error) {
	spec, err := ReadSpec(data)
	if err != nil {
		return Job{}, err
	}
	args := spec.Args
	job := Job{Binary: args[0], User: spec.User, Env: spec.Env}
	for i := 1; i+1 < len(args); i++ {
		value := args[i+1]
		switch args[i] {
		case "--server":
			job.Server = value
		case "--token":
			job.Token = value
		case "--connect-mode":
			job.ConnectMode = value
		case "--listen":
			job.Listen = value
		case "--state":
			job.StateDir = value
		default:
			continue
		}
		i++
	}
	if token, ok := job.Env[tokenEnv]; ok {
		job.Token = token
		delete(job.Env, tokenEnv)
	}
	return job, nil
}

// trimServer is a server URL as compared: without a trailing slash.
func trimServer(s string) string { return strings.TrimRight(strings.TrimSpace(s), "/") }
