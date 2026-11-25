package app

// MemberService is the separate answers a member can give. Liveness does not
// grant business traffic, and business readiness does not by itself make the
// member the scheduler.
type MemberService struct {
	Live          bool
	BusinessReady bool
	Schedulable   bool
	// BusinessRefusal is the refusal code of the first fact business is
	// missing; it is empty exactly when BusinessReady is true.
	BusinessRefusal string
}

// EvaluateMemberService keeps the three answers independent. Business traffic
// requires liveness, a material copy that survives one fault, a routable
// session, and a current target version. Scheduling authority is independent:
// a healthy follower can serve business requests through the cluster entry.
func EvaluateMemberService(live, materialsSurviveFault, schedulingLeader, sessionRoutable, targetCurrent bool) MemberService {
	refusal := ""
	switch {
	case !live:
		refusal = refusalNoHandler
	case !materialsSurviveFault:
		refusal = refusalProofNotEligible
	case !sessionRoutable:
		refusal = refusalSessionsUnroutable
	case !targetCurrent:
		refusal = refusalTargetNotCurrent
	}
	return MemberService{
		Live:            live,
		Schedulable:     live && schedulingLeader,
		BusinessReady:   refusal == "",
		BusinessRefusal: refusal,
	}
}
