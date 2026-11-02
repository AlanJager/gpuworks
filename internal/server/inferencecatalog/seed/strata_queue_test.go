package seed

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/pkg/catalogasset"
)

func TestStrataQueueAssetKeepsOriginalModelArguments(t *testing.T) {
	_, spec := catalogSpec(t, "ista-daslab/qwen3.8-flash-next-strata", "iq2xs-strata-nvidia-single-gpu")
	if len(spec.Assets) != 1 || spec.Assets[0].UnpackTo != "/opt/gpuworks/strata-queue" {
		t.Fatalf("queue runtime assets = %+v", spec.Assets)
	}
	asset := spec.Assets[0]
	data, err := embeddedCatalog.ReadFile("catalog/models/ista-daslab/qwen3.8-flash-next-strata/" + asset.File)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != asset.SHA256 {
		t.Fatal("catalog queue asset checksum differs from its embedded bytes")
	}
	if _, err := catalogasset.ValidateTarGzip(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	engine, err := deploy.SelectEngine(spec.Engine)
	if err != nil {
		t.Fatal(err)
	}
	launch, err := deploy.ResolveLaunch(deploy.ResolveLaunchInput{
		Engine: engine, Entrypoint: spec.Entrypoint, APIContract: spec.API,
		Params: deploy.Params{ModelPath: "/model", APIPort: 30000, TemplateArgs: spec.Args, TemplateEnv: spec.Env},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(launch.Entrypoint, []string{"/opt/strata/.venv/bin/python", "/opt/gpuworks/strata-queue/strata_queue.py"}) {
		t.Fatalf("queue launcher = %v", launch.Entrypoint)
	}
	if !reflect.DeepEqual(launch.Argv, []string{"--model", "/model", "--host", "0.0.0.0", "--port", "30000"}) {
		t.Fatalf("Strata model arguments changed: %v", launch.Argv)
	}
}
