package deploy

import "path"

// opaqueShellCommand recognizes a shell executing a command string from the
// final argv. It deliberately inspects only the wrapper shape, never the shell
// text, so runner selection and safety checks do not depend on parsing shell.
func opaqueShellCommand(entrypoint, argv []string) (string, bool) {
	if len(entrypoint) == 0 {
		return "", false
	}
	if shell := path.Base(entrypoint[0]); shell != "sh" && shell != "bash" {
		return "", false
	}
	args := append([]string(nil), entrypoint[1:]...)
	args = append(args, argv...)
	if len(args) < 2 {
		return "", false
	}
	switch args[0] {
	case "-c", "-lc", "-cl":
		return args[1], true
	case "--login":
		if len(args) >= 3 && args[1] == "-c" {
			return args[2], true
		}
	}
	return "", false
}
