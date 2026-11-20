package store

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// A cluster member assembles a new business generation on the store its
// withdrawn generation used. The successor's modules register the same
// holder source and must not be refused, and the old guard must be gone.
func TestReleaseNodeAdmissionLetsTheNextGenerationRegisterAgain(t *testing.T) {
	s := newTestStore(t)
	refused := messages.NewProductError("TEST_FLEET_FULL", "fleet is full", nil)
	s.AddNodeEnrollmentGuard(func(FleetCapacity) *messages.ProductError { return refused })
	if err := s.AddSchedulingHolder(newSetHolder("plugins", "")); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSchedulingHolder(newSetHolder("plugins", "")); !errors.Is(err, ErrHolderConflict) {
		t.Fatalf("second holder with the same source = %v, want ErrHolderConflict", err)
	}
	if err := s.CheckNodeEnrollment(); !errors.Is(err, ErrNodeEnrollmentRefused) {
		t.Fatalf("enrollment with the guard registered = %v, want a refusal", err)
	}

	s.ReleaseNodeAdmission()

	if err := s.CheckNodeEnrollment(); err != nil {
		t.Fatalf("enrollment after release = %v, want the withdrawn guard gone", err)
	}
	if err := s.AddSchedulingHolder(newSetHolder("plugins", "")); err != nil {
		t.Fatalf("holder after release = %v, want the next generation accepted", err)
	}
	if got := len(s.admission.schedulingHolders()); got != 1 {
		t.Fatalf("%d holders after release and one registration, want 1", got)
	}
}
