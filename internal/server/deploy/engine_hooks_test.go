package deploy

import (
	"reflect"
	"testing"
)

// A registered adapter that does not answer the whole hook contract falls back
// to the base defaults silently, which is exactly what "the engine says
// nothing" looks like. Every adapter therefore embeds BaseEngine; this is the
// seam that fails when a new adapter forgets to.
func TestRegisteredEnginesImplementTheHookContract(t *testing.T) {
	for _, name := range EngineNames() {
		engine, err := SelectEngine(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := engine.(EngineHooks); !ok {
			t.Errorf("engine %q does not implement EngineHooks", name)
		}
	}
}

// Every registered engine must answer the utilization every deployment is sized
// with through one of its own two layers: the dictionary row, or the profile
// field for engines that have no dictionary. DefaultGPUMemUtil has a last-resort
// fallback for engines nobody has declared yet, and a test that leaned on it
// would hide exactly the gap the callable regression was.
func TestRegisteredEnginesDeclareTheirOwnUtilizationDefault(t *testing.T) {
	for _, name := range EngineNames() {
		if spec, known := engineParams(name)["gpu_memory_utilization"]; known {
			if value, ok := toFloat(spec.Default); ok && value > 0 {
				continue
			}
		}
		engine, err := SelectEngine(name)
		if err != nil {
			t.Fatal(err)
		}
		if engine.Profile().GPUMemUtilDefault > 0 {
			continue
		}
		t.Errorf("engine %q declares no utilization default of its own; DefaultGPUMemUtil would fall back to the default engine's", name)
	}
}

// The defaults are the behavior the five separate type assertions used to fall
// back to one by one, so they are worth pinning: a capability nobody
// implements means "keep the launch as built, state every default, unset
// nothing, validate nothing, no TP flags", never a silent false.
func TestBaseEngineHooksAreTheDocumentedDefaults(t *testing.T) {
	launch := ResolvedLaunch{Entrypoint: []string{"engine", "serve"}, Argv: []string{"--port", "30000"}}
	base := hooks(plainEngine{})

	if got := base.FinalizeLaunch(launch); !reflect.DeepEqual(got, launch) {
		t.Fatalf("FinalizeLaunch changed the launch: %+v", got)
	}
	if !base.DefaultApplicable("dtype", LaunchShape{Entrypoint: launch.Entrypoint, Args: launch.Argv}) {
		t.Fatal("DefaultApplicable must not withhold a default by default")
	}
	if base.CanUnsetTemplateFlag("--kv-transfer-config", "{}") {
		t.Fatal("CanUnsetTemplateFlag must not claim an unknown composite flag")
	}
	if err := base.ValidateTopology(launch, Params{}); err != nil {
		t.Fatalf("ValidateTopology = %v, want no validation", err)
	}
	if got := base.SymbolizePreviewTP(launch, []string{"--tp", "2"}, nil); !reflect.DeepEqual(got, launch) {
		t.Fatalf("SymbolizePreviewTP changed the launch: %+v", got)
	}
	exe, params := base.SplitCommand(launch)
	if !reflect.DeepEqual(exe, launch.Entrypoint) || !reflect.DeepEqual(params, launch.Argv) {
		t.Fatalf("SplitCommand = %v/%v, want the launch's own entrypoint/argv", exe, params)
	}
	if flags := base.TPFlags(); len(flags) != 0 {
		t.Fatalf("TPFlags = %v, want none", flags)
	}
	if flags := base.PDConnectorFlags(); len(flags) != 0 {
		t.Fatalf("PDConnectorFlags = %v, want none", flags)
	}
}

// plainEngine is an adapter that only answers the required Engine interface,
// which is what an engine looks like before it embeds BaseEngine.
type plainEngine struct{}

func (plainEngine) Name() string     { return "plain" }
func (plainEngine) Profile() Profile { return Profile{Name: "plain"} }
func (plainEngine) Build(Params) ResolvedLaunch {
	return ResolvedLaunch{Entrypoint: []string{"plain"}}
}

// The sleep capability read on hot paths is the one the full profile
// declares, for every registered engine, the default and an unknown one.
func TestSleepCapsForEngineMatchesTheProfile(t *testing.T) {
	for _, name := range append(EngineNames(), "") {
		profile, err := ProfileForEngine(name)
		if err != nil {
			t.Fatal(err)
		}
		if got := SleepCapsForEngine(name); !reflect.DeepEqual(got, profile.Sleep) {
			t.Errorf("%q: SleepCapsForEngine = %+v, profile says %+v", name, got, profile.Sleep)
		}
	}
	if got := SleepCapsForEngine("no-such-engine"); !reflect.DeepEqual(got, SleepCaps{}) {
		t.Errorf("unknown engine has caps %+v", got)
	}
}
