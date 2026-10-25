package deploy

import "strings"

// memlockUnlimitedArgs lift the container's locked-memory limit entirely. The
// host RDMA network, a template spec's memlock_unlimited and Engram shared
// memory all use this one pair.
var memlockUnlimitedArgs = [2]string{"--ulimit", "memlock=-1:-1"}

// launchDockerArgs is the docker args a launch ends with: the deployment's own
// args, then the options the platform adds. A memlock ulimit is added at most
// once, and only when the deployment's own args state none: a deployer who
// states a locked-memory limit has chosen it, whatever the template spec
// (memlock_unlimited), RDMA network or Engram shared memory would have asked for.
func launchDockerArgs(own []string, p Params, memlockUnlimited bool) []string {
	args := withComputeNetworkDockerArgs(append([]string(nil), own...), p)
	if memlockUnlimited && !hasMemlockUlimit(args) {
		args = append(args, memlockUnlimitedArgs[0], memlockUnlimitedArgs[1])
	}
	return args
}

// hasMemlockUlimit answers whether docker args set the memlock ulimit, in
// either "--ulimit memlock=..." or "--ulimit=memlock=..." form (and the
// single-token "--ulimit memlock=..." the deploy form's editor allows).
func hasMemlockUlimit(args []string) bool {
	for index, arg := range args {
		if dockerOptionName(strings.TrimSpace(arg)) != "--ulimit" {
			continue
		}
		if name, _, _ := strings.Cut(ulimitValue(args, index), "="); name == "memlock" {
			return true
		}
	}
	return false
}

// ulimitValue answers the value of the --ulimit option at args[index]
// ("memlock=-1:-1"). Unlike dockerOptionValue it splits the single-token form
// at the space first, because the value of a ulimit itself contains "=".
func ulimitValue(args []string, index int) string {
	token := strings.TrimSpace(args[index])
	if rest, ok := strings.CutPrefix(token, "--ulimit="); ok {
		return strings.TrimSpace(rest)
	}
	if _, rest, ok := strings.Cut(token, " "); ok {
		return strings.TrimSpace(rest)
	}
	if index+1 < len(args) {
		return strings.TrimSpace(args[index+1])
	}
	return ""
}
