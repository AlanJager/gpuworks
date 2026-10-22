package deploy

import "sort"

// LaunchShape is what default applicability reads about one concrete launch:
// the entrypoint, the arguments that follow it (the template's, then the
// deployer's), and the engine release the runtime image carries
// (ImageEngineVersion), empty when nobody recorded one.
type LaunchShape struct {
	Entrypoint    []string
	Args          []string
	EngineVersion string
}

// EngineDecidesParameterKeys lists the keys whose dictionary default this
// launch does not state, leaving them to the engine
// (EngineHooks.DefaultApplicable): the launch may run a program, or a
// release, that refuses the flag. Nothing is hidden or refused on the
// strength of that judgement - it can be wrong - so the deployer can still
// turn each key on or off, and the form shows the unstated position as
// "automatic" instead of the dictionary default. Only keys that have a
// default are listed: a key without one is the engine's already.
func EngineDecidesParameterKeys(engineName string, launch LaunchShape) ([]string, error) {
	engine, err := SelectEngine(engineName)
	if err != nil {
		return nil, err
	}
	applicability := hooks(engine)
	var keys []string
	for key, spec := range engineParams(engine.Name()) {
		if spec.Default != nil && !applicability.DefaultApplicable(key, launch) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys, nil
}
