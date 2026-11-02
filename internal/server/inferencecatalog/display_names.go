package inferencecatalog

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Upper bounds on a spec's display names, in characters. A name is a short
// summary ("910C (A3) · INT8 W8A8 · 16 卡"), not a parameter list; the longest
// built-in names are well under these.
const (
	maxDisplayNameZHCN = 56
	maxDisplayNameEN   = 72
)

func validateDisplayName(modelID, specName, field, value string, limit int) error {
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("model %s spec %s %s must be one line", modelID, specName, field)
	}
	if n := utf8.RuneCountInString(value); n > limit {
		return fmt.Errorf("model %s spec %s %s has %d characters; at most %d", modelID, specName, field, n, limit)
	}
	return nil
}

// validateDisplayNamesDistinct rejects two specs of one model and one engine
// that would show under the same name: the template page lists a model's
// specs per engine, and two equal rows there cannot be told apart. Case and
// runs of spaces do not make names different.
func validateDisplayNamesDistinct(model *InferenceModel) error {
	type key struct{ engine, field, name string }
	seen := map[key]string{}
	for _, spec := range model.Specs {
		for _, named := range [...]struct{ field, name string }{{"display_name_zh_cn", spec.DisplayNameZHCN}, {"display_name_en", spec.DisplayNameEN}} {
			field, name := named.field, named.name
			if name == "" {
				continue
			}
			k := key{spec.Engine, field, strings.ToLower(strings.Join(strings.Fields(name), " "))}
			if other, ok := seen[k]; ok {
				return fmt.Errorf("model %s specs %s and %s of engine %s share %s %q", model.ModelID, other, spec.Name, spec.Engine, field, name)
			}
			seen[k] = spec.Name
		}
	}
	return nil
}
