package templateprobe

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func calibrationFixtureInput() CalibrationRecord {
	return CalibrationRecord{
		ModelID: "Qwen/Qwen3-4B", ArchFamily: "dense", ExpectedMinLayers: 2,
		FullResult: ProbeOutcome{Status: "pass"}, TruncatedResult: ProbeOutcome{Status: "pass"},
		ImageDigest: "sha256:164afad43171dd431f0344d9808e42777ccfd1af2264e2409a8326ca4da509f4",
		Chip:        "K100_AI", CalibratedAt: "2026-08-27T12:10:19Z", Engine: "vLLM", Source: "hardware",
	}
}

func TestNewCalibrationRecordRequiresHardwareScope(t *testing.T) {
	input := calibrationFixtureInput()
	input.Source = "unit_test"
	if _, err := NewCalibrationRecord(input); err == nil {
		t.Fatal("non-hardware calibration was accepted")
	}
	input = calibrationFixtureInput()
	input.ImageDigest = "latest"
	if _, err := NewCalibrationRecord(input); err == nil {
		t.Fatal("mutable image reference was accepted")
	}
}

func TestNewCalibrationRecordDerivesFalseNegative(t *testing.T) {
	input := calibrationFixtureInput()
	input.FullResult = ProbeOutcome{Status: "fail", SignatureID: "engine.architecture.unregistered"}
	input.TruncatedResult = ProbeOutcome{Status: "pass"}
	record, err := NewCalibrationRecord(input)
	if err != nil {
		t.Fatal(err)
	}
	if !record.Alignment.FalseNegative || !record.DisableTruncation || record.Alignment.Aligned {
		t.Fatalf("record = %+v", record)
	}
}

func TestWriteCalibrationFixturePublishesValidatedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "calibration.json")
	if err := WriteCalibrationFixture(path, calibrationFixtureInput()); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got CalibrationRecord
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Source != "hardware" || !got.Alignment.Aligned || !strings.HasSuffix(string(body), "\n") {
		t.Fatalf("fixture = %+v body=%q", got, body)
	}
}
