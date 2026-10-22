package deploy

import "testing"

const gib int64 = 1 << 30

func card(name string, totalGiB int64) CardFacts {
	return CardFacts{Name: name, MemoryTotalBytes: totalGiB * gib}
}

// What a deployer sees in the form on their own card. These are the figures the
// change exists to produce, so they are written out rather than recomputed from
// the rule - a case that re-derived the formula would agree with any formula.
func TestMaterializeDefaultsTheUtilizationToTheCard(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cards []CardFacts
		want  float64
	}{
		{"a 12 GiB card", []CardFacts{card("GeForce RTX 3080", 12)}, 0.83},
		{"an 11 GiB card", []CardFacts{card("GeForce RTX 2080 Ti", 11)}, 0.81},
		{"a 16 GiB card", []CardFacts{card("Tesla T4", 16)}, 0.87},
		{"a 24 GiB card keeps the engine default", []CardFacts{card("GeForce RTX 4090", 24)}, 0.9},
		{"an 80 GiB card keeps the engine default", []CardFacts{card("A100-SXM4-80GB", 80)}, 0.9},
		{"unified memory", []CardFacts{card("GB10", 128)}, 0.8},
		{"no cards at all", nil, 0.9},
		{"a card nobody measured", []CardFacts{{Name: "GeForce RTX 3080"}}, 0.9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Materialize(MaterializeInput{Engine: EngineVLLM, Cards: tc.cards})
			if err != nil {
				t.Fatal(err)
			}
			if out.Quick.GPUMemUtil != tc.want {
				t.Fatalf("GPUMemUtil = %v, want %v", out.Quick.GPUMemUtil, tc.want)
			}
		})
	}
}

// SGLang's own default is 0.88, so the cap has to be read against the engine's
// row rather than vLLM's: a 16 GiB card caps it, a 24 GiB card does not.
func TestTheCapIsTakenAgainstTheEnginesOwnDefault(t *testing.T) {
	for _, tc := range []struct {
		cards []CardFacts
		want  float64
	}{
		{[]CardFacts{card("Tesla T4", 16)}, 0.87},
		{[]CardFacts{card("GeForce RTX 4090", 24)}, 0.88},
		{nil, 0.88},
	} {
		out, err := Materialize(MaterializeInput{Engine: EngineSGLang, Cards: tc.cards})
		if err != nil {
			t.Fatal(err)
		}
		if out.Quick.GPUMemUtil != tc.want {
			t.Fatalf("sglang on %+v = %v, want %v", tc.cards, out.Quick.GPUMemUtil, tc.want)
		}
	}
}

// One share is applied to every card the deployment uses, so the smallest card
// decides. Taking any other card would leave the small one short of the reserve
// the rule exists to keep.
func TestTheSmallestCardDecides(t *testing.T) {
	mixed := []CardFacts{
		card("Quadro RTX 8000", 48),
		card("GeForce RTX 2080 Ti", 11),
		card("Quadro RTX 8000", 48),
	}
	out, err := Materialize(MaterializeInput{Engine: EngineVLLM, Cards: mixed})
	if err != nil {
		t.Fatal(err)
	}
	if out.Quick.GPUMemUtil != 0.81 {
		t.Fatalf("mixed node = %v, want the 11 GiB card's 0.81", out.Quick.GPUMemUtil)
	}
	// A card with no measured capacity must not be mistaken for a small one.
	withUnmeasured := append([]CardFacts{{Name: "GeForce RTX 3080"}}, card("Quadro RTX 8000", 48))
	out, err = Materialize(MaterializeInput{Engine: EngineVLLM, Cards: withUnmeasured})
	if err != nil {
		t.Fatal(err)
	}
	if out.Quick.GPUMemUtil != 0.9 {
		t.Fatalf("an unmeasured card capped the share to %v", out.Quick.GPUMemUtil)
	}
}

// The deployer's own value is not a default, and nothing here may touch it -
// including a value above what the reserve would allow, which is their card to
// fill.
func TestAStatedUtilizationIsNeverCapped(t *testing.T) {
	out, err := Materialize(MaterializeInput{
		Engine: EngineVLLM,
		Raw:    map[string]any{"gpu_memory_utilization": 0.95},
		Cards:  []CardFacts{card("GeForce RTX 3080", 12)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Quick.GPUMemUtil != 0.95 {
		t.Fatalf("GPUMemUtil = %v, want the stated 0.95", out.Quick.GPUMemUtil)
	}
}

// A card too small to hold the reserve has no share to offer, but the row still
// has to stay inside its own published bounds: a zero would read back as "never
// stated" and a negative is not a value at all. Admission refuses such a card
// for capacity, which can say so.
func TestACardTooSmallForTheReserveFallsToTheRowsFloor(t *testing.T) {
	out, err := Materialize(MaterializeInput{Engine: EngineVLLM, Cards: []CardFacts{card("GeForce GTX 1050", 2)}})
	if err != nil {
		t.Fatal(err)
	}
	spec := engineParams(EngineVLLM)["gpu_memory_utilization"]
	if spec.Min == nil {
		t.Fatal("the dictionary row lost its minimum; this case has nothing to measure against")
	}
	if out.Quick.GPUMemUtil != *spec.Min {
		t.Fatalf("GPUMemUtil = %v, want the row's floor %v", out.Quick.GPUMemUtil, *spec.Min)
	}
}

// The capped value reaches the engine as the decimal the form shows. The share
// is a step count times a step, and that product carries float error: a command
// line reading 0.8300000000000001 is the same number and a different artifact.
func TestTheCappedShareReachesArgvAsItsDecimal(t *testing.T) {
	out, err := Materialize(MaterializeInput{Engine: EngineVLLM, Cards: []CardFacts{card("GeForce RTX 3080", 12)}})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := SelectEngine(EngineVLLM)
	if err != nil {
		t.Fatal(err)
	}
	launch := engine.Build(Params{ModelPath: "/model", ServedName: "m", APIPort: 8000, Quick: out.Quick})
	found := ""
	for i, arg := range launch.Argv {
		if arg == "--gpu-memory-utilization" && i+1 < len(launch.Argv) {
			found = launch.Argv[i+1]
		}
	}
	if found != "0.83" {
		t.Fatalf("argv carried %q, want \"0.83\": %v", found, launch.Argv)
	}
}

// The rule changes one row. A card-dependent cap leaking into another key would
// silently rewrite a default nobody connected to memory.
func TestCappingTouchesOnlyTheUtilizationRow(t *testing.T) {
	plain := engineParams(EngineVLLM)
	adapted := capDefaultsToCards(plain, []CardFacts{card("GeForce RTX 3080", 12)})
	for key, want := range plain {
		got := adapted[key]
		if key == "gpu_memory_utilization" {
			if got.Default == want.Default {
				t.Fatalf("the utilization row was not capped at all")
			}
			continue
		}
		if got.Default != want.Default {
			t.Errorf("%s default changed from %v to %v", key, want.Default, got.Default)
		}
	}
	// The row's own bounds are not the place to express a card: a deployer may
	// still state anything the dictionary publishes.
	before, after := plain["gpu_memory_utilization"], adapted["gpu_memory_utilization"]
	if *after.Min != *before.Min || *after.Max != *before.Max || *after.Step != *before.Step {
		t.Fatalf("capping moved the row's bounds: min/max/step %v/%v/%v -> %v/%v/%v",
			*before.Min, *before.Max, *before.Step, *after.Min, *after.Max, *after.Step)
	}
	// The caller's map is shared with the rest of the process; adapting it in
	// place would cap every later reader on every engine.
	if plain["gpu_memory_utilization"].Default != 0.9 {
		t.Fatalf("the dictionary itself was mutated: default is now %v", plain["gpu_memory_utilization"].Default)
	}
}

// DefaultGPUMemUtil answers for the other case: nothing was written down, so no
// flag is emitted and the engine runs at its own figure whatever card it is on.
// Capping it would make the estimate describe a share the running engine is not
// using, which is why it takes no cards - a reader who adds some would be
// "fixing" it in the one direction that breaks it.
func TestTheEnginesOwnDefaultIsNotACardDerivedOne(t *testing.T) {
	if got := DefaultGPUMemUtil(EngineVLLM); got != 0.9 {
		t.Fatalf("vllm = %v, want its own 0.9", got)
	}
	if got := DefaultGPUMemUtil(EngineSGLang); got != 0.88 {
		t.Fatalf("sglang = %v, want its own 0.88", got)
	}
	// The dictionary it reads is the shared one, so capping a deployment's copy
	// must not have moved it.
	if _, err := Materialize(MaterializeInput{Engine: EngineVLLM, Cards: []CardFacts{card("GeForce RTX 3080", 12)}}); err != nil {
		t.Fatal(err)
	}
	if got := DefaultGPUMemUtil(EngineVLLM); got != 0.9 {
		t.Fatalf("vllm = %v after a capped materialization, want its own 0.9", got)
	}
}
