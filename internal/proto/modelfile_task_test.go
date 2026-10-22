package proto

import (
	"encoding/json"
	"reflect"
	"testing"
)

// Path and RelPath travel beside the old target_path, which an agent that
// predates them still reads; whichever of the two is unset is absent.
func TestModelFileTaskPathFieldsWireContract(t *testing.T) {
	for _, tc := range []struct {
		name string
		task ModelFileTask
		want string
	}{
		{"absolute", ModelFileTask{Key: "1-2", Path: "/data/models/x", TargetPath: "/data/models/x"},
			`{"key":"1-2","model_id":0,"source":"","source_id":"","revision":"","path":"/data/models/x","target_path":"/data/models/x","desired_state":"","rate_limit_kbs":0}`},
		{"relative", ModelFileTask{Key: "1-2", RelPath: "Qwen/Qwen3-8B", TargetPath: "Qwen/Qwen3-8B"},
			`{"key":"1-2","model_id":0,"source":"","source_id":"","revision":"","rel_path":"Qwen/Qwen3-8B","target_path":"Qwen/Qwen3-8B","desired_state":"","rate_limit_kbs":0}`},
	} {
		raw, err := json.Marshal(tc.task)
		if err != nil || string(raw) != tc.want {
			t.Errorf("%s: wire=%s err=%v, want %s", tc.name, raw, err, tc.want)
		}
		var got ModelFileTask
		if err := json.Unmarshal(raw, &got); err != nil || !reflect.DeepEqual(got, tc.task) {
			t.Errorf("%s: round trip=%+v err=%v", tc.name, got, err)
		}
	}
	// A task from a server that predates the fields has only target_path.
	var old ModelFileTask
	if err := json.Unmarshal([]byte(`{"key":"1-2","target_path":"/data/models/x"}`), &old); err != nil ||
		old.Path != "" || old.RelPath != "" || old.TargetPath != "/data/models/x" {
		t.Errorf("old task = %+v err=%v", old, err)
	}
}

// Candidates travel beside the endpoint an agent that predates them reads.
func TestModelFileTaskCandidatesWireContract(t *testing.T) {
	task := ModelFileTask{Key: "1-2", Source: "huggingface", SourceID: "Qwen/Qwen3-8B", Endpoint: "https://hf-mirror.com",
		Candidates: []ModelFetchCandidate{{Base: "https://hf-mirror.com"}, {Base: "https://huggingface.co", Upstream: true}}}
	want := `{"key":"1-2","model_id":0,"source":"huggingface","source_id":"Qwen/Qwen3-8B","revision":"","target_path":"","desired_state":"","rate_limit_kbs":0,` +
		`"candidates":[{"base":"https://hf-mirror.com"},{"base":"https://huggingface.co","upstream":true}],"endpoint":"https://hf-mirror.com"}`
	raw, err := json.Marshal(task)
	if err != nil || string(raw) != want {
		t.Fatalf("wire=%s err=%v, want %s", raw, err, want)
	}
	var got ModelFileTask
	if err := json.Unmarshal(raw, &got); err != nil || !reflect.DeepEqual(got, task) {
		t.Fatalf("round trip=%+v err=%v", got, err)
	}
	var old ModelFileTask
	if err := json.Unmarshal([]byte(`{"key":"1-2","endpoint":"https://hf-mirror.com"}`), &old); err != nil || old.Candidates != nil {
		t.Fatalf("a task from an older server = %+v err=%v", old, err)
	}
}
