package deploy

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
)

// LaunchArgs are the tokens a resolved launch runs, with a manual command's
// shell script parsed into its own tokens (the launch is then `/bin/sh -c
// <script>`). scriptArgs are those script tokens alone, nil otherwise. ok is
// false when the script does not parse.
func LaunchArgs(launch proto.ResolvedLaunch) (args, scriptArgs []string, ok bool) {
	args = append(append([]string(nil), launch.Entrypoint...), launch.Argv...)
	if len(launch.Argv) == 2 && launch.Argv[0] == "-c" && len(launch.Entrypoint) == 1 && launch.Entrypoint[0] == "/bin/sh" {
		tokens, err := ParseArgsText(launch.Argv[1])
		if err != nil {
			return nil, nil, false
		}
		scriptArgs = tokens
		args = append(args, tokens...)
	}
	return args, scriptArgs, true
}

// LaunchFlag is the value the last occurrence of flag takes in args, as
// `flag value` or `flag=value`. A value the shell would still expand ($ or a
// backtick) is not one the launch fixes, so it is not reported.
func LaunchFlag(args []string, flag string) (string, bool) {
	for index := len(args) - 1; index >= 0; index-- {
		arg := args[index]
		value, found := "", false
		if arg == flag && index+1 < len(args) {
			value, found = args[index+1], true
		} else if strings.HasPrefix(arg, flag+"=") {
			value, found = strings.TrimPrefix(arg, flag+"="), true
		}
		if found {
			if strings.ContainsAny(value, "$`") {
				return "", false
			}
			return value, true
		}
	}
	return "", false
}

// ServedModelName is the name a launch tells the engine to answer under
// (--served-model-name), or "" when it sets none.
func ServedModelName(launch proto.ResolvedLaunch) string {
	args, _, ok := LaunchArgs(launch)
	if !ok {
		return ""
	}
	name, _ := LaunchFlag(args, "--served-model-name")
	return name
}
