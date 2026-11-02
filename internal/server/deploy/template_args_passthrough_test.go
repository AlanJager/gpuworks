package deploy

import (
	"slices"
	"testing"
)

// A template value the parameter dictionary does not know - a KV cache type
// a newer engine added, say - reaches the engine exactly as written, once,
// and the platform does not add its own value for the same flag.
func TestTemplateValueTheDictionaryCannotReadReachesTheEngineVerbatim(t *testing.T) {
	for _, engineName := range []string{EngineVLLM, EngineSGLang} {
		engine, err := SelectEngine(engineName)
		if err != nil {
			t.Fatal(err)
		}
		args := []string{"--kv-cache-dtype", "fp3_from_the_future", "--max-model-len", "4096"}
		launch, err := ResolveLaunch(ResolveLaunchInput{
			Engine: engine,
			Params: Params{ModelPath: "/model", APIPort: 30000, TensorParallel: 1, TemplateArgs: args},
		})
		if err != nil {
			t.Fatalf("%s: %v", engineName, err)
		}
		argv := append(append([]string{}, launch.Entrypoint...), launch.Argv...)
		count := 0
		for i, token := range argv {
			if token == "--kv-cache-dtype" {
				count++
				if i+1 >= len(argv) || argv[i+1] != "fp3_from_the_future" {
					t.Errorf("%s: --kv-cache-dtype followed by %v", engineName, argv[i+1:])
				}
			}
		}
		if count != 1 || !slices.Contains(argv, "4096") {
			t.Errorf("%s argv = %v", engineName, argv)
		}
	}
}
