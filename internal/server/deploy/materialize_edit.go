package deploy

import "slices"

// EditedParams is what an edit of a stored configuration states beyond it:
// the request's quick_params whose value differs from base, and the unset
// keys base actually has. A field that restates its stored value is the
// deployment's own (MaterializeInput.Base), so this is the layer an edit is
// held to the dictionary's rules for, as a create is for what it states.
func EditedParams(engine string, base LaunchConfig, raw map[string]any, unset []string) (map[string]any, []string, error) {
	selected, err := SelectEngine(engine)
	if err != nil {
		return nil, nil, err
	}
	params := engineParams(selected.Name())
	stored, other, err := baseValues(params, &base)
	if err != nil {
		return nil, nil, err
	}
	edited := map[string]any{}
	for key, value := range raw {
		if value == nil {
			continue
		}
		spec, known := params[key]
		if !known {
			if previous, kept := other[key]; !kept || !sameValue(previous, value) {
				edited[key] = value
			}
			continue
		}
		canonical, ok := canonicalValue(spec, value)
		if !ok {
			edited[key] = value // invalid as sent: validation names it
			continue
		}
		if previous, kept := stored[key]; !kept || !sameValue(previous, canonical) {
			edited[key] = value
		}
	}
	var removed []string
	for _, key := range unset {
		if _, kept := stored[key]; kept && !slices.Contains(removed, key) {
			removed = append(removed, key)
		}
	}
	return edited, removed, nil
}
