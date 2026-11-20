package inferencecatalog

import (
	"errors"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// Every container path a spec claims is checked against every other by one
// rule, whatever kind of mount each is, and the refusal is a stable code with
// both paths and their fields as parameters.
func TestSpecContainerPathsShareOneOverlapRule(t *testing.T) {
	base := func() *InferenceSpec {
		return &InferenceSpec{
			Name:   "tp4",
			Assets: []Asset{{File: "assets/a.tar", UnpackTo: "/opt/kit"}, {File: "assets/b.tar", UnpackTo: "/opt/kit"}},
			Dirs:   []deploy.ManagedDir{{Name: "state", MountPath: "/state", LimitBytes: 1}, {Name: "data", MountPath: "/gpuworks-data", LimitBytes: 1}},
		}
	}
	if err := validateContainerPaths("Org/Model", base()); err != nil {
		t.Fatalf("assets sharing one directory and a separate dir: %v", err)
	}
	for name, test := range map[string]struct {
		mutate            func(*InferenceSpec)
		field, otherField string
	}{
		"asset inside the model mount": {func(s *InferenceSpec) { s.Assets[0].UnpackTo = "/model/kit" }, "asset assets/a.tar unpack_to", "the model mount"},
		// /gpuworks is the platform's (the peers file, the pre-start
		// wrapper): one more claim, refused by the same rule.
		"asset in the platform directory": {func(s *InferenceSpec) { s.Assets[1].UnpackTo = "/gpuworks/kit" }, "asset assets/b.tar unpack_to", "the platform directory"},
		"dir on the platform directory":   {func(s *InferenceSpec) { s.Dirs[0].MountPath = "/gpuworks" }, "dir state mount_path", "the platform directory"},
		"component on the peers file": {func(s *InferenceSpec) {
			s.Components = []Component{{Name: "main", Role: "primary", MountPath: "/model"}, {Name: "draft", Role: "speculative_draft", MountPath: "/gpuworks/peers.json"}}
		}, "component draft mount_path", "the platform directory"},
		"dir around an asset": {func(s *InferenceSpec) { s.Dirs[0].MountPath = "/opt" }, "dir state mount_path", "asset assets/a.tar unpack_to"},
		"dir on the primary component": {func(s *InferenceSpec) {
			s.Components = []Component{{Name: "main", Role: "primary", MountPath: "/weights"}}
			s.Dirs[0].MountPath = "/weights/cache"
		}, "dir state mount_path", "the model mount"},
	} {
		t.Run(name, func(t *testing.T) {
			spec := base()
			test.mutate(spec)
			err := validateContainerPaths("Org/Model", spec)
			var product *messages.ProductError
			if !errors.As(err, &product) || product.Code() != messages.ErrorCatalogMountPathConflict {
				t.Fatalf("err = %v, want %s", err, messages.ErrorCatalogMountPathConflict)
			}
			if product.Params()["field"] != test.field || product.Params()["other_field"] != test.otherField {
				t.Fatalf("params = %+v", product.Params())
			}
			want := map[string]any{"model": "Org/Model", "spec": "tp4", "field": test.field, "path": product.Params()["path"], "other_field": test.otherField, "other_path": product.Params()["other_path"]}
			if !reflect.DeepEqual(product.Params(), want) {
				t.Fatalf("params = %+v, want keys %+v", product.Params(), want)
			}
		})
	}
}
