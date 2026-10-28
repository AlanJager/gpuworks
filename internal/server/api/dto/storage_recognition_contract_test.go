package dto

import (
	"encoding/json"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestStorageRecognitionWireContracts(t *testing.T) {
	for _, tt := range []struct {
		name, wire string
		target     any
	}{
		{"agent candidate", `{"rel_path":"org/repo/FL2VA","size_bytes":42,"files":3,"source":"modelscope","source_id":"org/repo","revision":"master","subfolder":"FL2VA","format":"diffusers","tasks":["text-to-video"],"metadata_issues":["external_component"],"basis":"modelscope","incomplete":true,"incomplete_reason":"missing encoder"}`, &proto.StorageScanCandidate{}},
		{"browser candidate", `{"rel_path":"org/repo/FL2VA","size_bytes":42,"files":3,"source":"modelscope","source_id":"org/repo","revision":"master","subfolder":"FL2VA","format":"diffusers","tasks":["text-to-video"],"metadata_issues":["external_component"],"basis":"modelscope","incomplete":true,"incomplete_reason":"missing encoder","imported":false}`, &StorageScanCandidate{}},
		{"browser selection", `{"rel_path":"org/repo/FL2VA","source":"modelscope","source_id":"org/repo","revision":"master","subfolder":"FL2VA","size_bytes":42}`, &StorageImportItem{}},
		{"old candidate", `{"rel_path":"old","size_bytes":0,"files":0,"source":"local","source_id":"old","basis":"path","imported":false}`, &StorageScanCandidate{}},
		{"old selection", `{"rel_path":"old"}`, &StorageImportItem{}},
		{"import conflict", `{"rel_path":"org/model","error":"Path occupied","error_detail":{"code":"MODEL_FILE_PATH_OCCUPIED","params":{"storage_id":2,"model_id":7,"path":"org/model"},"message":"Path occupied"}}`, &StorageImported{}},
		{"legacy import error", `{"rel_path":"org/model","error":"Cannot read directory"}`, &StorageImported{}},
		{"import success", `{"rel_path":"org/model","model_id":7,"created":true}`, &StorageImported{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := json.Unmarshal([]byte(tt.wire), tt.target); err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(tt.target)
			if err != nil {
				t.Fatal(err)
			}
			var got, want any
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tt.wire), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("wire changed: %s want %s", b, tt.wire)
			}
		})
	}
}
