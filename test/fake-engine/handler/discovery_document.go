package handler

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// The fake engine implements transcription in addition to the captured
// vLLM-Omni routes. Describe that handler without changing the real capture.
//
//go:embed testdata/openapi/fake-transcription-path.json
var transcriptionPath []byte

var discoveryDocument = buildDiscoveryDocument()

func buildDiscoveryDocument() []byte {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(openAPIDocument, &document); err != nil {
		panic(fmt.Errorf("decode captured fake-engine schema: %w", err))
	}
	var paths map[string]json.RawMessage
	if err := json.Unmarshal(document["paths"], &paths); err != nil {
		panic(fmt.Errorf("decode captured fake-engine paths: %w", err))
	}
	paths["/v1/audio/transcriptions"] = transcriptionPath
	var err error
	document["paths"], err = json.Marshal(paths)
	if err != nil {
		panic(fmt.Errorf("encode fake-engine paths: %w", err))
	}
	result, err := json.Marshal(document)
	if err != nil {
		panic(fmt.Errorf("encode fake-engine schema: %w", err))
	}
	return result
}
