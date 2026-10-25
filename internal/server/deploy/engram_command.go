package deploy

import (
	"regexp"
	"strings"
)

// engramLaunch is what the launch command says about Engram. The checks around
// it guard against a misconfigured shared-memory setup; they do not police
// commands that have nothing to do with Engram, so a command that never
// mentions it is not inspected at all.
type engramLaunch struct {
	// args are the words the Engram options are read from.
	args  []string
	state engramArgumentState
	// unchecked is set when a shell command mentions Engram but its words
	// cannot be read statically, so no setting could be derived from it.
	unchecked bool
}

var engramConfigText = regexp.MustCompile(`(?i)engram[-_]config`)

func mentionsEngram(text string) bool {
	return strings.Contains(strings.ToLower(text), "engram")
}

// inspectEngramLaunch reads Engram options from the final command. A command
// string run by sh or bash is read as the static words it spells out; one that
// uses expansions, or hides the option inside a nested shell string, is
// reported as unchecked rather than guessed at.
func inspectEngramLaunch(entrypoint, argv []string) engramLaunch {
	command := append(append([]string(nil), entrypoint...), argv...)
	if !mentionsEngram(strings.Join(command, "\n")) {
		return engramLaunch{}
	}
	shell := inspectShellCommand(entrypoint, argv)
	if !shell.commandString || !mentionsEngram(shell.script) {
		return engramLaunch{args: command, state: inspectEngramArguments(command)}
	}
	words, err := staticShellWords(shell.script)
	if err != nil {
		return engramLaunch{unchecked: true}
	}
	state := inspectEngramArguments(words)
	return engramLaunch{
		args: words, state: state,
		unchecked: !state.present && engramConfigText.MatchString(shell.script),
	}
}
