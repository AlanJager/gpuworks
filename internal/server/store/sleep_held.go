package store

// Holding a sleep is a fact of each instance (Instance.SleepHeld): an
// operator's or a schedule's sleep holds exactly the instances it orders, a
// wake lifts the hold of exactly the instances it wakes or leaves waiting
// for room, and a sleep the platform chose (the residency arbiter's) neither
// sets nor clears it. A deployment-wide sleep order (deployments.
// desired_sleep) is always held: only an operator or a schedule orders a
// whole deployment to sleep; the platform names the replicas it puts to
// sleep.

// WakesOnRequest says a request for the deployment wakes in, one of its
// sleepers, rather than being refused: the deployment can sleep, and in's
// sleep is the platform's choice rather than an operator's or a schedule's.
// The gateway, the residency arbiter, placement, batches and the playground
// all ask this one question, per sleeper; what they say of a whole
// deployment is derived from it (planning.RequestWakes).
func (d *Deployment) WakesOnRequest(in *Instance) bool {
	return d != nil && in != nil && d.Sleepable && !in.SleepHeld
}
