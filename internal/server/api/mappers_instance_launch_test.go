package api

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// An instance's launch fields come from its desired state alone: the image
// catalog carries no launch configuration to compare against (ADR 0031), so the
// wire object has no image-defaults snapshot, current value, diff or staleness.
func TestInstanceDTOReportsTheLaunchFromItsDesiredState(t *testing.T) {
	st, err := store.Open("file:api_mapper_instance_launch?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("mapper-launch-node", store.HashToken("mapper-launch-token"), "mapper-launch-node")
	if err != nil {
		t.Fatal(err)
	}
	image, err := st.UpsertImage(&store.Image{ImageRef: "example/mapper:v1", Source: "community", Engine: "vllm", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "mapper-launch", NodeID: node.ID, Image: image.ImageRef, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	desired := `{"launch":{"command_mode":"image-entry","argv":["--port","30000"],"env":{"A":"1"},"docker_args":["--ipc=host"],"shm_bytes":34359738368,"model_mount":"/models"},"uuid":"mapper-launch-instance"}`
	instance, err := st.CreateInstance(&store.Instance{UUID: "mapper-launch-instance", DeploymentID: dep.ID, NodeID: node.ID, State: "running", Image: image.ImageRef, DesiredStateJSON: desired})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal((&apiHandlers{d: Deps{Store: st}}).instanceDTO(instance))
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"command_mode":     `"image-entry"`,
		"model_mount_path": `"/models"`,
		"docker_args":      `["--ipc=host"]`,
		"shm_bytes":        `34359738368`,
		"env":              `{"A":"1"}`,
	} {
		if string(wire[key]) != want {
			t.Fatalf("%s = %s, want %s (wire %s)", key, wire[key], want, encoded)
		}
	}
	for _, retired := range []string{"image_defaults", "current_image_defaults", "image_defaults_diff", "image_defaults_stale", "container_overrides"} {
		if _, present := wire[retired]; present {
			t.Fatalf("instance wire still carries %s: %s", retired, encoded)
		}
	}
}
