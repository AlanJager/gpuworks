package selfinstall

import (
	"bytes"
	"errors"
	"os"
	"reflect"

	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

// Apply only the semantic changes made by installConfig to the original YAML.
// Comparing typed configurations avoids a second list of installer-owned keys
// and keeps unmodified operator fields, explicit defaults and comments intact.
func (i *Installer) installConfigPayload(config serverconfig.File) ([]byte, error) {
	payload, err := serverconfig.Render(config)
	if err != nil || !i.layout.inSharedRoot() {
		return payload, err
	}
	old, err := os.ReadFile(i.layout.ConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return payload, nil
	}
	if err != nil {
		return nil, err
	}
	previous, err := serverconfig.Load(i.layout.ConfigPath())
	if err != nil {
		return nil, err
	}
	previousPayload, err := serverconfig.Render(previous)
	if err != nil {
		return nil, err
	}
	var existing, replacement, prior yaml.Node
	if err := yaml.Unmarshal(previousPayload, &prior); err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(old, &existing); err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(payload, &replacement); err != nil {
		return nil, err
	}
	if len(existing.Content) != 1 || existing.Content[0].Kind != yaml.MappingNode {
		return payload, nil
	}
	mergeConfigChanges(existing.Content[0], prior.Content[0], replacement.Content[0])
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(&existing); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
func configMappingIndex(node *yaml.Node, key string) int {
	for n := 0; n+1 < len(node.Content); n += 2 {
		if node.Content[n].Value == key {
			return n
		}
	}
	return -1
}

// Patch semantic differences recursively, including removed mapping keys.
// Original explicit defaults absent from both renders remain unchanged.
func mergeConfigChanges(original, before, after *yaml.Node) {
	if sameConfigValue(before, after) {
		return
	}
	if original.Kind == yaml.MappingNode && before.Kind == yaml.MappingNode && after.Kind == yaml.MappingNode {
		var keys []string
		seen := map[string]bool{}
		for _, mapping := range []*yaml.Node{before, after} {
			for n := 0; n+1 < len(mapping.Content); n += 2 {
				key := mapping.Content[n].Value
				if !seen[key] {
					keys = append(keys, key)
					seen[key] = true
				}
			}
		}
		for _, key := range keys {
			oldIndex, newIndex := configMappingIndex(before, key), configMappingIndex(after, key)
			if oldIndex >= 0 && newIndex >= 0 && sameConfigValue(before.Content[oldIndex+1], after.Content[newIndex+1]) {
				continue
			}
			index := configMappingIndex(original, key)
			switch {
			case newIndex < 0:
				if index >= 0 {
					original.Content = append(original.Content[:index], original.Content[index+2:]...)
				}
			case index < 0:
				original.Content = append(original.Content, after.Content[newIndex:newIndex+2]...)
			case oldIndex >= 0:
				mergeConfigChanges(original.Content[index+1], before.Content[oldIndex+1], after.Content[newIndex+1])
			default:
				replaceConfigValue(original.Content[index+1], after.Content[newIndex+1])
			}
		}
		return
	}
	replaceConfigValue(original, after)
}
func replaceConfigValue(original, next *yaml.Node) {
	next.HeadComment, next.LineComment, next.FootComment = original.HeadComment, original.LineComment, original.FootComment
	*original = *next
}

func sameConfigValue(before, after *yaml.Node) bool {
	var previous, next any
	return before.Decode(&previous) == nil && after.Decode(&next) == nil && reflect.DeepEqual(previous, next)
}
