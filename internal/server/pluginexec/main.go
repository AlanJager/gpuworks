package pluginexec

import (
	"errors"
	"log/slog"
	"os"
	"runtime"
	"syscall"
)

// errIsolation marks a refusal to start without the isolation the profile
// requires.
var errIsolation = errors.New("the required isolation is not available on this host")

// Main runs the trampoline for the arguments after the subcommand and, on
// success, never returns: the process becomes the plugin. It returns an
// exit code when it could not get that far. Diagnostics are JSON lines on
// stderr, which the server forwards to its log.
func Main(args []string) int {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("component", Command)
	p, argv, err := ParseArgs(args)
	if err != nil {
		log.Error("cannot read the command line", "err", err)
		return ExitUsage
	}
	log = log.With("plugin", p.ID)
	// Every per-thread attribute set below (the parent-death signal,
	// no_new_privs, the Landlock domain, nice) belongs to the thread that
	// calls execve; keep this goroutine on it.
	runtime.LockOSThread()
	if err := prepare(p, log); err != nil {
		if errors.Is(err, errIsolation) {
			log.Error("refusing to start the plugin", "code", IsolationCode, "required", "landlock", "err", err)
			return ExitIsolationUnavailable
		}
		log.Error("cannot apply the sandbox", "err", err)
		return ExitSetupFailed
	}
	env := filterEnv(os.Environ(), allowed())
	err = syscall.Exec(argv[0], argv, env)
	log.Error("cannot execute the plugin", "exe", argv[0], "err", err)
	return ExitExecFailed
}
