package modelscan

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestScanLargeIndexThroughLocalFileView(t *testing.T) {
	root := t.TempDir()
	write(t, p(root, "repo/config.json"), `{"architectures":["ExampleForCausalLM"]}`)
	write(t, p(root, "repo/shard.safetensors"), "weights")
	var index strings.Builder
	index.WriteString(`{"weight_map":{`)
	for i := 0; i < 100000; i++ {
		if i > 0 {
			index.WriteByte(',')
		}
		fmt.Fprintf(&index, "%q:%q", fmt.Sprintf("layer.%d.%s.weight", i, strings.Repeat("expert.", 24)), "shard.safetensors")
	}
	index.WriteString("}}")
	if index.Len() <= 16<<20 {
		t.Fatal("fixture must exceed the old aggregate read budget")
	}
	write(t, p(root, "repo/model.safetensors.index.json"), index.String())
	r, err := Scan(context.Background(), root, Options{})
	if err != nil || len(r.Candidates) != 1 || r.Candidates[0].Incomplete || r.Candidates[0].Format != "transformers" {
		t.Fatalf("large index scan=%+v err=%v", r, err)
	}
}

func TestScanReadLimitIsNotInvalidMetadata(t *testing.T) {
	root := t.TempDir()
	write(t, p(root, "repo/config.json"), strings.Repeat(" ", 3<<20))
	write(t, p(root, "repo/model.safetensors"), "weights")
	r, err := Scan(context.Background(), root, Options{})
	if err != nil || len(r.Candidates) != 0 || skippedMap(r)["repo"] != proto.ScanSkipMetadataLimit {
		t.Fatalf("limit mislabeled: %+v %v", r, err)
	}
	if !proto.ScanSkipUndecided(proto.ScanSkipMetadataLimit) {
		t.Fatal("a read limit cannot prove registered weights are gone")
	}
}
