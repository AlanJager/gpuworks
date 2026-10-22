package templateprobe

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type fakeL3Deployer struct {
	handle       DeploymentHandle
	observation  DeploymentObservation
	createErr    error
	waitErr      error
	cleanupErr   error
	cleanupCalls int
}

func (f *fakeL3Deployer) Create(context.Context, json.RawMessage) (DeploymentHandle, error) {
	return f.handle, f.createErr
}

func (f *fakeL3Deployer) WaitReady(context.Context, DeploymentHandle) (DeploymentObservation, error) {
	return f.observation, f.waitErr
}

func (f *fakeL3Deployer) Cleanup(context.Context, DeploymentHandle) error {
	f.cleanupCalls++
	return f.cleanupErr
}

type fakeL3Evaluator struct {
	result TaskEvaluation
	err    error
}

func (f fakeL3Evaluator) Evaluate(context.Context, DeploymentObservation, L3Case) (TaskEvaluation, error) {
	return f.result, f.err
}

func readyObservation() DeploymentObservation {
	return DeploymentObservation{
		DeploymentID: 9, BaseURL: "http://gpuworks.test", Model: "served-model",
		ImageStarted: true, WeightsLoaded: true, Healthy: true,
		ImageDigest: "sha256:abc", EnvironmentMap: map[string]string{"OMP_NUM_THREADS": "1"},
		EngineArgv: []string{"vllm", "serve", "/model"},
	}
}

func validTextCase() L3Case {
	return L3Case{CaseID: "case-1", Task: L3TaskText, Prompt: "Return four", ExpectedKeywords: []string{"4"}}
}

func TestL3SemanticFailureIsWarningNotPlatformFailure(t *testing.T) {
	deployer := &fakeL3Deployer{handle: DeploymentHandle{ID: 9}, observation: readyObservation()}
	runner := L3Runner{
		Deployer: deployer,
		Evaluator: fakeL3Evaluator{result: TaskEvaluation{
			RequestCompleted: true, HTTPStatus: 200, SemanticPassed: false,
			Warning: "assistant output is repeated punctuation",
		}},
		CleanupTimeout: time.Second,
	}
	result, err := runner.Run(context.Background(), L3Input{
		Deployment: json.RawMessage(`{"model_id":1}`), Case: validTextCase(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != L3StatusWarn || !result.PlatformCompatible || result.Verification.SemanticOutputValidation != "WARN" {
		t.Fatalf("result = %+v", result)
	}
	if deployer.cleanupCalls != 1 || !result.Cleanup.Attempted || !result.Cleanup.Completed {
		t.Fatalf("cleanup = %+v, calls=%d", result.Cleanup, deployer.cleanupCalls)
	}
}

func TestL3PlatformAndHarnessFailuresRemainDistinctAndCleanUp(t *testing.T) {
	tests := []struct {
		name       string
		waitErr    error
		evalResult TaskEvaluation
		evalErr    error
		wantStatus L3Status
	}{
		{name: "runtime failed", waitErr: PlatformStageError{Stage: "model_load", Err: errors.New("engine exited")}, wantStatus: L3StatusFail},
		{name: "management API unavailable", waitErr: HarnessStageError{Stage: "deployment_poll", Err: errors.New("bad response")}, wantStatus: L3StatusError},
		{name: "request not completed", evalResult: TaskEvaluation{HTTPStatus: 500, RequestError: "HTTP 500"}, wantStatus: L3StatusFail},
		{name: "invalid local fixture", evalErr: HarnessStageError{Stage: "evaluation_fixture", Err: errors.New("missing WAV")}, wantStatus: L3StatusError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deployer := &fakeL3Deployer{handle: DeploymentHandle{ID: 9}, observation: readyObservation(), waitErr: tt.waitErr}
			result, err := (L3Runner{
				Deployer: deployer, Evaluator: fakeL3Evaluator{result: tt.evalResult, err: tt.evalErr}, CleanupTimeout: time.Second,
			}).Run(context.Background(), L3Input{Deployment: json.RawMessage(`{"model_id":1}`), Case: validTextCase()})
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != tt.wantStatus || result.PlatformCompatible {
				t.Fatalf("result = %+v, want status %s", result, tt.wantStatus)
			}
			if deployer.cleanupCalls != 1 {
				t.Fatalf("cleanup calls = %d", deployer.cleanupCalls)
			}
		})
	}
}

func TestL3KeepDeploymentIsExplicit(t *testing.T) {
	deployer := &fakeL3Deployer{handle: DeploymentHandle{ID: 9}, observation: readyObservation()}
	result, err := (L3Runner{
		Deployer:  deployer,
		Evaluator: fakeL3Evaluator{result: TaskEvaluation{RequestCompleted: true, HTTPStatus: 200, SemanticPassed: true}},
	}).Run(context.Background(), L3Input{
		Deployment: json.RawMessage(`{"model_id":1}`), Case: validTextCase(), KeepDeployment: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != L3StatusPass || deployer.cleanupCalls != 0 || result.Cleanup.Attempted || !result.Cleanup.Retained {
		t.Fatalf("result = %+v, cleanup calls = %d", result, deployer.cleanupCalls)
	}
}

func TestL3CanceledEvaluationIsHarnessErrorAndCleansUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	deployer := &fakeL3Deployer{handle: DeploymentHandle{ID: 9}, observation: readyObservation()}
	result, err := (L3Runner{
		Deployer:       deployer,
		Evaluator:      fakeL3Evaluator{result: TaskEvaluation{RequestError: context.Canceled.Error()}},
		CleanupTimeout: time.Second,
	}).Run(ctx, L3Input{Deployment: json.RawMessage(`{"model_id":1}`), Case: validTextCase()})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != L3StatusError || result.Verification.FailureStage != "canceled" || result.PlatformCompatible || deployer.cleanupCalls != 1 {
		t.Fatalf("result=%+v cleanup calls=%d", result, deployer.cleanupCalls)
	}
}
