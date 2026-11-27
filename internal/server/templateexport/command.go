package templateexport

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	placementeval "gpuworks.ai/gpuworks/internal/server/placement"
)

// Command is the part of a deployment's command a spec can state: an
// entrypoint that replaces the engine's executable, or a manual shell script
// the spec owns whole (InferenceSpec.ManualCommand). The zero value is the
// engine's own command, which the spec's args describe.
type Command struct {
	// Entrypoint replaces the executable of the engine-built command
	// (container_overrides.entrypoint_display in engine mode).
	Entrypoint []string
	// ManualCommand is the deployment's manual shell script, as written.
	ManualCommand string
	// IncludesPreparation says the script owns preparation
	// (container_overrides.command_includes_preparation): the spec then
	// states no pre-start step of its own.
	IncludesPreparation bool
}

// CommandOf is the command of a deployment with overrides, or the reason no
// spec field states it. Image-entry mode keeps the image's own executable,
// which a spec has no field for; a complete argv (cmd_display in engine
// mode, or a JSON argv in manual mode) is not the engine's command plus args,
// and a spec's manual_command is a shell script, not an argv. A manual script
// written for a model mounted somewhere other than the platform's mount
// states that path, which the spec does not carry.
func CommandOf(overrides deploy.ContainerOverrides) (Command, error) {
	cmd := strings.TrimSpace(overrides.CmdDisplay)
	switch deploy.CommandMode(overrides.CommandMode) {
	case deploy.CommandImageEntry:
		return Command{}, Unavailable(ReasonCommandImageEntry, nil)
	case deploy.CommandManual:
		if cmd == "" {
			// No command: the image runs its own.
			return Command{}, Unavailable(ReasonCommandImageEntry, nil)
		}
		if strings.HasPrefix(cmd, "[") {
			return Command{}, Unavailable(ReasonCommandFullArgv, nil)
		}
		if mount := strings.TrimSpace(overrides.ModelMountPath); mount != "" && path.Clean(mount) != deploy.DefaultModelMount {
			return Command{}, Unavailable(ReasonCommandModelMount, map[string]any{"model_mount_path": overrides.ModelMountPath})
		}
		// Extra mounts describe one host and are not exported: a script that
		// reaches into one would not find it on a deployment of the spec.
		for _, m := range overrides.ExtraMounts {
			if target := path.Clean(strings.TrimSpace(m.ContainerPath)); target != "." && target != "/" && strings.Contains(cmd, target) {
				return Command{}, Unavailable(ReasonCommandMount, map[string]any{"mount_path": target})
			}
		}
		return Command{ManualCommand: cmd, IncludesPreparation: overrides.CommandIncludesPreparation}, nil
	}
	if cmd != "" {
		return Command{}, Unavailable(ReasonCommandFullArgv, nil)
	}
	if strings.TrimSpace(overrides.EntrypointDisplay) == "" {
		return Command{}, nil
	}
	entrypoint, err := deploy.ParseCommandDisplay(overrides.EntrypointDisplay)
	if err != nil {
		return Command{}, fmt.Errorf("container overrides entrypoint: %w", err)
	}
	return Command{Entrypoint: entrypoint}, nil
}

// Stated reports whether the command is not the engine's own.
func (c Command) Stated() bool {
	return len(c.Entrypoint) > 0 || c.ManualCommand != ""
}

// Replay is overrides and selection as a deployment of the exported spec
// starts them: the deployment's own command fields are gone and the spec's
// entrypoint, pre-start steps and manual command take their place, the way
// the create path applies them (deployment.applySpecManualCommand). Building
// the launch from the result and comparing it with the instance's proves the
// spec reproduces the command.
func (c Command) Replay(overrides deploy.ContainerOverrides, selection deploy.CatalogSelection) (deploy.ContainerOverrides, deploy.CatalogSelection) {
	overrides.EntrypointDisplay, overrides.CmdDisplay, overrides.CommandMode = "", "", ""
	overrides.CommandIncludesPreparation = false
	if len(c.Entrypoint) > 0 {
		selection.Entrypoint = append([]string(nil), c.Entrypoint...)
	}
	if c.ManualCommand != "" {
		overrides.CommandMode = string(deploy.CommandManual)
		overrides.CmdDisplay = c.ManualCommand
		if c.IncludesPreparation {
			selection.PreStartArgv, selection.PreStartScript = nil, ""
		}
	}
	return overrides, selection
}

// applyCommand writes c into spec. pinned reports a placement
// pinned to the cards the script ran on: the script states its parallelism
// itself, so the spec cannot be scheduled on another card count. A script
// that is the source spec's own manual_command keeps the source's
// placement, which its author wrote for it.
func applyCommand(spec *inferencecatalog.InferenceSpec, c Command, sourceManual string, cards int) (pinned bool, err error) {
	if len(c.Entrypoint) > 0 {
		spec.Entrypoint = append([]string(nil), c.Entrypoint...)
	}
	if c.ManualCommand == "" {
		return false, nil
	}
	if c.IncludesPreparation {
		spec.PreStartArgv, spec.PreStartScript = nil, ""
	}
	spec.ManualCommand = c.ManualCommand
	if c.ManualCommand != strings.TrimSpace(sourceManual) {
		if spec.Placement, err = pinCards(spec.Placement, cards); err != nil {
			return false, err
		}
		pinned = true
	}
	return pinned, nil
}

// redactCommand replaces the spec's manual command and entrypoint by the
// redacted form a logged launch takes and reports whether that changed them:
// the result then holds no secret, and no longer runs as it is.
func redactCommand(spec *inferencecatalog.InferenceSpec) bool {
	changed := false
	if spec.ManualCommand != "" {
		redacted := deploy.RedactLaunchText(spec.ManualCommand)
		changed = redacted != spec.ManualCommand
		spec.ManualCommand = redacted
	}
	if len(spec.Entrypoint) > 0 {
		redacted := deploy.RedactLaunchArgv(spec.Entrypoint)
		if !slices.Equal(redacted, spec.Entrypoint) {
			changed = true
			spec.Entrypoint = redacted
		}
	}
	return changed
}

// withheldValue is the shortest env value looked for as a literal in a
// command: a shorter one (a flag value such as "1") says nothing about where
// it came from.
const withheldValue = 4

// shellAssignment matches the keys a shell command assigns or exports.
var shellAssignment = regexp.MustCompile(`(?m)(?:^|[\s;&|(])(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)=`)

// assigned is the env keys a command assigns: in its manual script, and as
// KEY=value words of its entrypoint (env KEY=value cmd).
func assigned(spec inferencecatalog.InferenceSpec) map[string]bool {
	keys := map[string]bool{}
	for _, m := range shellAssignment.FindAllStringSubmatch(spec.ManualCommand, -1) {
		keys[m[1]] = true
	}
	for _, word := range spec.Entrypoint {
		if m := shellAssignment.FindStringSubmatch(word); m != nil && strings.HasPrefix(word, m[1]+"=") {
			keys[m[1]] = true
		}
	}
	return keys
}

// withholdEnv redacts, in the spec's command, the values of the deployment's
// env keys the export leaves out (excluded) that the command writes out: it
// assigns the key, or holds the value as a literal. It returns those keys,
// sorted. The deploy form's editor seeds a manual script with an export line
// for every launch env key, so without this a script would carry exactly the
// values the export reports as left out.
func withholdEnv(spec *inferencecatalog.InferenceSpec, user map[string]string, excluded []string) []string {
	if !(Command{ManualCommand: spec.ManualCommand, Entrypoint: spec.Entrypoint}).Stated() || len(excluded) == 0 {
		return nil
	}
	keys := assigned(*spec)
	var withheld []string
	for _, key := range excluded {
		value := user[key]
		literal := len(value) >= withheldValue && (strings.Contains(spec.ManualCommand, value) || slices.ContainsFunc(spec.Entrypoint, func(word string) bool { return strings.Contains(word, value) }))
		if !keys[key] && !literal {
			continue
		}
		withheld = append(withheld, key)
		if value != "" {
			spec.ManualCommand = strings.ReplaceAll(spec.ManualCommand, value, deploy.RedactedValue)
			for i, word := range spec.Entrypoint {
				spec.Entrypoint[i] = strings.ReplaceAll(word, value, deploy.RedactedValue)
			}
		}
	}
	sort.Strings(withheld)
	return withheld
}

// platformEnvSet is the keys the spec's command assigns that the platform set
// for this node in the instance's launch (its system env layer): the command
// fixes this node's values for them.
func platformEnvSet(spec inferencecatalog.InferenceSpec, in Input) []string {
	if len(in.PlatformEnv) == 0 {
		return nil
	}
	var keys []string
	for key := range assigned(spec) {
		if _, set := in.PlatformEnv[key]; set {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// pinCards is source (nil for none) accepting only count cards on one node.
// Any other answer is the source resolver's own, so the variables and memory
// contract the deployment ran with stay what they were.
func pinCards(source *inferencecatalog.Placement, count int) (*inferencecatalog.Placement, error) {
	resolver := ""
	if source != nil {
		resolver = source.Resolver
	}
	pinned, err := placementeval.PinCount(resolver, count, fmt.Sprintf("the manual command was tested on exactly %d card(s) of one node", count))
	if err != nil {
		return nil, Unavailable(ReasonCommandNotImportable, nil)
	}
	return &inferencecatalog.Placement{Language: "starlark", Resolver: pinned}, nil
}
