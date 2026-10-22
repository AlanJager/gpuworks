package deploy

import (
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/netplan"
)

func memlockArgs(args []string) []string {
	var out []string
	for i, arg := range args {
		if dockerOptionName(arg) == "--ulimit" {
			if value := ulimitValue(args, i); strings.HasPrefix(value, "memlock") {
				out = append(out, value)
			}
		}
	}
	return out
}

// A spec that sets memlock_unlimited starts its container with unlimited
// locked memory, unless the deployment's own docker args state a memlock
// ulimit; and whatever asks for it, the launch carries exactly one.
func TestResolveLaunchMemlockUnlimited(t *testing.T) {
	rdma := Params{NNodes: 2, ComputeNetwork: ComputeNetwork{Type: netplan.TypeHostRDMA}}
	accelerator := Params{NNodes: 2, ComputeNetwork: ComputeNetwork{Type: netplan.TypeAcceleratorRDMA, HCAs: []string{"npu3"}}}
	for name, test := range map[string]struct {
		params Params
		field  bool
		own    []string
		want   []string // every memlock ulimit in the final docker args
	}{
		"field true adds one":                  {field: true, want: []string{"memlock=-1:-1"}},
		"field false adds none":                {},
		"field false keeps the user's":         {own: []string{"--ulimit", "memlock=1024"}, want: []string{"memlock=1024"}},
		"user two-token form wins":             {field: true, own: []string{"--ulimit", "memlock=65536:65536"}, want: []string{"memlock=65536:65536"}},
		"user equals form wins":                {field: true, own: []string{"--ulimit=memlock=65536"}, want: []string{"memlock=65536"}},
		"user single-token form wins":          {field: true, own: []string{"--ulimit memlock=-1"}, want: []string{"memlock=-1"}},
		"user's own unlimited is not repeated": {field: true, own: []string{"--ulimit", "memlock=-1:-1"}, want: []string{"memlock=-1:-1"}},
		"another ulimit does not count":        {field: true, own: []string{"--ulimit", "stack=67108864"}, want: []string{"memlock=-1:-1"}},
		"host RDMA alone":                      {params: rdma, want: []string{"memlock=-1"}},
		"host RDMA with the field is one":      {params: rdma, field: true, want: []string{"memlock=-1"}},
		"host RDMA keeps the user's limit":     {params: rdma, field: true, own: []string{"--ulimit", "memlock=4096"}, want: []string{"memlock=4096"}},
		"host RDMA, no field, user's limit":    {params: rdma, own: []string{"--ulimit=memlock=4096"}, want: []string{"memlock=4096"}},
		"accelerator network needs none":       {params: accelerator},
		"accelerator network with the field":   {params: accelerator, field: true, want: []string{"memlock=-1:-1"}},
	} {
		t.Run(name, func(t *testing.T) {
			params := test.params
			params.ModelPath, params.APIPort, params.AssignedGPUs, params.TensorParallel = "/model", 30000, 1, 1
			launch, err := ResolveLaunch(ResolveLaunchInput{
				Engine: launchTestEngine{}, Params: params, TemplateMemlockUnlimited: test.field,
				ContainerOverrides: ContainerOverrides{DockerArgs: test.own},
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := memlockArgs(launch.DockerArgs); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("memlock ulimits = %v in %v, want %v", got, launch.DockerArgs, test.want)
			}
		})
	}
}

// The deployer's args are copied, never edited in place.
func TestLaunchDockerArgsDoesNotMutateTheDeploymentsArgs(t *testing.T) {
	own := make([]string, 2, 8)
	own[0], own[1] = "--cap-add", "SYS_NICE"
	got := launchDockerArgs(own, Params{}, true)
	if want := []string{"--cap-add", "SYS_NICE", "--ulimit", "memlock=-1:-1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %v", got)
	}
	if !reflect.DeepEqual(own, []string{"--cap-add", "SYS_NICE"}) || len(own[:cap(own)]) != 8 || own[:4][2] != "" {
		t.Fatalf("deployment args changed: %v", own[:4])
	}
}

// The snapshot a deployment keeps carries the flag, and one written before the
// field existed reads as false.
func TestCatalogSelectionCarriesMemlockUnlimited(t *testing.T) {
	selection, err := DecodeCatalogSelection([]byte(`{"engine":"vllm","spec_name":"deepinfer","spec_source":"upstream","memlock_unlimited":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !selection.MemlockUnlimited {
		t.Fatal("memlock_unlimited was not decoded")
	}
	encoded, err := selection.Encode()
	if err != nil || !strings.Contains(string(encoded), `"memlock_unlimited":true`) {
		t.Fatalf("encoded = %s, %v", encoded, err)
	}
	old, err := DecodeCatalogSelection([]byte(`{"engine":"vllm","spec_name":"balanced","spec_source":"upstream"}`))
	if err != nil || old.MemlockUnlimited {
		t.Fatalf("old snapshot = %+v, %v", old, err)
	}
	if encoded, err := old.Encode(); err != nil || strings.Contains(string(encoded), "memlock") {
		t.Fatalf("old snapshot re-encoded as %s, %v", encoded, err)
	}
}
