package deploy

import (
	"reflect"
	"testing"
)

// One implementation owns the model-path rewrite: engine builders emit a
// positional path, a --model/--model-path value pair, or an equals form, and a
// configurable mount has to reach all of them (moved here from the controller,
// which kept a second copy of the same rewrite).
func TestReplaceModelPathArgsSupportsAllEngineForms(t *testing.T) {
	got := ReplaceModelPathArgs([]string{"serve", "/model", "--model", "/model", "--model-path=/model", "--other=/model"}, "/model", "/models")
	want := []string{"serve", "/models", "--model", "/models", "--model-path=/models", "--other=/model"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReplaceModelPathArgs() = %#v, want %#v", got, want)
	}
}
