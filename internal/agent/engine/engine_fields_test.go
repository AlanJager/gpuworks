package engine

import (
	"reflect"
	"testing"
)

// Every Engine field is either per-instance state - kept by instance UUID,
// with the lifetime it is dropped at - or said here not to be. A new field
// fails this test until it is classified, so per-instance state cannot be
// added without deciding when it goes (ADR 0066). The target is one record
// per instance; the per-instance maps still separate are listed so the next
// change can fold them in.
func TestEveryEngineFieldIsClassified(t *testing.T) {
	perInstance := map[string]string{
		// The engine process's facts, one record, dropped where the
		// container is confirmed gone (reclaimInstance) and on a runtime
		// switch: TestANewContainerUnderTheSameUUIDKnowsNothingOfTheOldProcess.
		"processes": "the engine process: dropped by reclaimInstance and a runtime switch",
		// The desired generation and its reconcile attempt.
		"desired":         "the desired generation: dropped when the server removes the instance",
		"inFlight":        "the reconcile attempt: dropped when the attempt ends",
		"inFlightCancel":  "the reconcile attempt: dropped when the attempt ends",
		"pending":         "the reconcile attempt: dropped when the attempt ends or the instance is removed",
		"cleanupRequired": "the old generation: dropped once its container is reclaimed",
		// Attempt guards.
		"runtimeEpoch": "kept for the UUID's life in this agent on purpose: a removal moves it to a fresh global epoch, so a reused UUID never matches an old attempt",
		"servingEpoch": "the attempt that reported the instance running: dropped when that attempt is invalidated or its container exits",
		"healthProbes": "the running health probe: dropped when it is cancelled",
	}
	notPerInstance := map[string]string{
		"driver": "a dependency", "hostRuntimeFacts": "a dependency", "prober": "a dependency",
		"report": "a dependency", "log": "a dependency", "runtimeMounts": "a dependency",
		"extractLibvgpu": "a dependency", "checkPorts": "a dependency",
		"captureObservationConnection": "a dependency", "reportRuntimeObservation": "a dependency",
		"networkInterfaceName": "a dependency", "networkConfig": "a dependency", "logDrain": "a dependency",
		"puller": "a dependency", "otherRuntimes": "a dependency", "cardGate": "a dependency",
		"stateDir": "configuration", "hostRoot": "configuration", "residueSettle": "configuration",
		"headlessProbeInterval": "configuration", "headlessStableChecks": "configuration",
		"livenessInterval": "configuration", "healthProbeAttempts": "configuration",
		"healthProbeInterval": "configuration", "healthProbeTimeout": "configuration",
		"mu":               "guards the per-instance state",
		"assetMu":          "serializes content-addressed cache publication",
		"partitionMu":      "serializes per-card virtual-id allocation",
		"desiredKnown":     "whether the server has sent the full desired set once",
		"nextRuntimeEpoch": "the global epoch counter",
	}
	seen := map[string]bool{}
	ty := reflect.TypeOf(Engine{})
	for i := 0; i < ty.NumField(); i++ {
		name := ty.Field(i).Name
		seen[name] = true
		if perInstance[name] == "" && notPerInstance[name] == "" {
			t.Errorf("Engine.%s is not classified: if it is kept by instance, put it in the instance's record for the lifetime it shares (processFacts for the engine process) and say when it goes; otherwise say why here", name)
		}
	}
	for _, list := range []map[string]string{perInstance, notPerInstance} {
		for name := range list {
			if !seen[name] {
				t.Errorf("the classification lists %s, which Engine no longer has", name)
			}
		}
	}
}
