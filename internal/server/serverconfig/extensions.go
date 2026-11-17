package serverconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Section decodes extensions.<name> of server.yaml into v. Unknown keys in
// the section are an error, as they are at the top level. An absent section
// leaves v as it is, so a module keeps its defaults.
func (f File) Section(name string, v any) error {
	node, ok := f.Extensions[name]
	if !ok {
		return nil
	}
	raw, err := yaml.Marshal(&node)
	if err != nil {
		return fmt.Errorf("extensions.%s: %w", name, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("extensions.%s: %s", name, strings.TrimPrefix(err.Error(), "yaml: "))
	}
	return nil
}

// UnknownSections are the names under extensions that none of known claims,
// sorted. The server warns about them rather than refusing to start: a
// section left behind by another build is not an error of this one.
func UnknownSections(f File, known []string) []string {
	var out []string
	for name := range f.Extensions {
		if !slices.Contains(known, name) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}
