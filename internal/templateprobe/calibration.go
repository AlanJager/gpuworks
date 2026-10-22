package templateprobe

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// CalibrationRecord is the durable fact used to regression-test truncation.
// Source is deliberately explicit: fixture files may only contain real
// hardware executions, never unit-test or synthetic outcomes.
type CalibrationRecord struct {
	ModelID           string          `json:"model_id"`
	ArchFamily        string          `json:"arch_family"`
	ConfigFields      map[string]any  `json:"config_fields"`
	ExpectedMinLayers int             `json:"expected_min_layers"`
	FullResult        ProbeOutcome    `json:"full_result"`
	TruncatedResult   ProbeOutcome    `json:"truncated_result"`
	ImageDigest       string          `json:"image_digest"`
	Chip              string          `json:"chip"`
	CalibratedAt      string          `json:"calibrated_at"`
	Engine            string          `json:"engine,omitempty"`
	Source            string          `json:"source"`
	Alignment         AlignmentResult `json:"alignment"`
	DisableTruncation bool            `json:"disable_truncation"`
}

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// NewCalibrationRecord validates the evidence scope and computes the
// full/truncated alignment before anything can be persisted.
func NewCalibrationRecord(input CalibrationRecord) (CalibrationRecord, error) {
	input.ModelID = strings.TrimSpace(input.ModelID)
	input.ArchFamily = strings.TrimSpace(input.ArchFamily)
	input.ImageDigest = strings.TrimSpace(input.ImageDigest)
	input.Chip = strings.TrimSpace(input.Chip)
	input.Engine = strings.ToLower(strings.TrimSpace(input.Engine))
	input.Source = strings.ToLower(strings.TrimSpace(input.Source))
	if input.ModelID == "" || input.ArchFamily == "" {
		return CalibrationRecord{}, errors.New("model_id and arch_family are required")
	}
	if input.ExpectedMinLayers <= 0 {
		return CalibrationRecord{}, errors.New("expected_min_layers must be positive")
	}
	if !digestPattern.MatchString(input.ImageDigest) {
		return CalibrationRecord{}, fmt.Errorf("image_digest must be an immutable sha256 digest")
	}
	if input.Chip == "" {
		return CalibrationRecord{}, errors.New("chip is required")
	}
	if input.Source != "hardware" {
		return CalibrationRecord{}, errors.New("calibration source must be hardware")
	}
	if input.FullResult.Status != "pass" && input.FullResult.Status != "fail" {
		return CalibrationRecord{}, errors.New("full_result.status must be pass or fail")
	}
	if input.TruncatedResult.Status != "pass" && input.TruncatedResult.Status != "fail" {
		return CalibrationRecord{}, errors.New("truncated_result.status must be pass or fail")
	}
	calibratedAt := strings.TrimSpace(input.CalibratedAt)
	if calibratedAt == "" {
		calibratedAt = time.Now().UTC().Format(time.RFC3339)
	} else if _, err := time.Parse(time.RFC3339, calibratedAt); err != nil {
		return CalibrationRecord{}, fmt.Errorf("calibrated_at must be RFC3339: %w", err)
	}
	input.CalibratedAt = calibratedAt
	input.Alignment = CompareOutcomes(input.FullResult, input.TruncatedResult)
	input.DisableTruncation = input.Alignment.FalseNegative
	return input, nil
}

// WriteCalibrationFixture atomically writes one validated hardware record.
// The destination is caller-owned and is never created from a probe test.
func WriteCalibrationFixture(path string, input CalibrationRecord) error {
	record, err := NewCalibrationRecord(input)
	if err != nil {
		return err
	}
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal calibration fixture: %w", err)
	}
	body = append(body, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(path), ".calibration-*")
	if err != nil {
		return fmt.Errorf("create calibration fixture: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(body); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("publish calibration fixture: %w", err)
	}
	return nil
}
