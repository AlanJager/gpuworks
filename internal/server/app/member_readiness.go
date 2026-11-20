package app

// MemberService is the separate answers a member can give. Liveness does not
// grant business traffic, and business readiness does not by itself make the
// member the scheduler.
type MemberService struct {
	Live          bool
	BusinessReady bool
	Schedulable   bool
}

// EvaluateMemberService keeps the three answers independent. Business traffic
// requires liveness, a material copy that survives one fault, a routable
// session, and a current target version. Scheduling authority is independent:
// a healthy follower can serve business requests through the cluster entry.
func EvaluateMemberService(live, materialsSurviveFault, schedulingLeader, sessionRoutable, targetCurrent bool) MemberService {
	schedulable := live && schedulingLeader
	return MemberService{
		Live:          live,
		Schedulable:   schedulable,
		BusinessReady: live && materialsSurviveFault && sessionRoutable && targetCurrent,
	}
}
