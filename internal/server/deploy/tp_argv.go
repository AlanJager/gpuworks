package deploy

import "strconv"

// TensorParallelFromArgv reads the tensor-parallel degree a launch command
// states for the named engine, using that engine's own flag spellings
// (TPFlagsForEngine). ok is false when the engine states none, the flag is
// absent, or its value is not a positive integer (a callable recipe, whose
// parallel scheme the platform does not know, always reads as unknown).
func TensorParallelFromArgv(engine string, argv []string) (tp int, ok bool) {
	flags := TPFlagsForEngine(engine)
	for i, arg := range argv {
		for _, flag := range flags {
			value := ""
			switch {
			case len(arg) > len(flag) && arg[:len(flag)+1] == flag+"=":
				value = arg[len(flag)+1:]
			case arg == flag && i+1 < len(argv):
				value = argv[i+1]
			default:
				continue
			}
			if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
				return parsed, true
			}
		}
	}
	return 0, false
}
