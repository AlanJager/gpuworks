package inferencecatalog

import (
	"encoding/json"
	"testing"
)

// Turning a stored model back into a portable catalog model refuses a
// weight_sources column that does not decode, naming the spec and the column,
// and reads a blank one as no sources.
func TestFromFlatStoreModelWeightSourcesColumnShapes(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      string // JSON of the restored sources, or the error text
	}{
		{name: "blank", raw: "", want: `null`},
		{name: "whitespace", raw: "  ", want: `null`},
		{name: "null", raw: "null", want: `null`},
		{name: "empty", raw: "[]", want: `[]`},
		{name: "valid", raw: `[{"provider":"huggingface","repo_id":"Org/Model","subfolder":"w"}]`,
			want: `[{"provider":"huggingface","repo_id":"Org/Model","subfolder":"w"}]`},
		{name: "undecodable", raw: "not json",
			want: "decode stored inference catalog org/model/bf16-balanced weight_sources: invalid character 'o' in literal null (expecting 'u')"},
		{name: "partial", raw: `[{"provider":"huggingface","repo_id":"Org/Model"},{"repo_id":5}]`,
			want: "decode stored inference catalog org/model/bf16-balanced weight_sources: json: cannot unmarshal number into Go struct field WeightSource.repo_id of type string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := ToFlatStoreModels(flatFixture())
			if err != nil {
				t.Fatal(err)
			}
			rows[0].Specs[0].WeightSources = tc.raw
			restored, err := FromFlatStoreModel(rows[0])
			got := ""
			if err != nil {
				got = err.Error()
			} else {
				encoded, _ := json.Marshal(restored.Specs[0].Weights.Sources)
				got = string(encoded)
			}
			if got != tc.want {
				t.Fatalf("FromFlatStoreModel weight sources = %s, want %s", got, tc.want)
			}
		})
	}
}

// DecodeWeightSources is the one decoder of inference_specs.weight_sources. A
// blank column declares nothing; a value that does not decode is an error that
// still carries what encoding/json had filled in, which is what the callers
// that tolerate a bad column have always read.
func TestDecodeWeightSources(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string // want: JSON of the result
		wantErr         bool
	}{
		{name: "blank", raw: "", want: `null`},
		{name: "whitespace", raw: " \n\t", want: `null`},
		{name: "null", raw: "null", want: `null`},
		{name: "empty", raw: "[]", want: `[]`},
		{name: "valid", raw: ` [ {"provider":"modelscope","repo_id":"Org/M","revision":"r","subfolder":"s"} ] `,
			want: `[{"provider":"modelscope","repo_id":"Org/M","revision":"r","subfolder":"s"}]`},
		{name: "undecodable", raw: "not json", want: `null`, wantErr: true},
		{name: "not a list", raw: `{"repo_id":"Org/M"}`, want: `null`, wantErr: true},
		{name: "partial", raw: `[{"provider":"huggingface","repo_id":"Org/M"},{"provider":"modelscope","repo_id":5}]`,
			want: `[{"provider":"huggingface","repo_id":"Org/M"},{"provider":"modelscope","repo_id":""}]`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sources, err := DecodeWeightSources(tc.raw)
			encoded, _ := json.Marshal(sources)
			if string(encoded) != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("DecodeWeightSources(%q) = %s, %v; want %s, error %v", tc.raw, encoded, err, tc.want, tc.wantErr)
			}
		})
	}
}
