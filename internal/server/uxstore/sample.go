package uxstore

import "gpuworks.ai/gpuworks/internal/server/uxrecord"

// keepSlowMS: over the row budget, a request whose first output took at
// least this long is always kept.
const keepSlowMS int32 = 10000

// waitMS is how long the user waited for the first output: the first token
// of any kind, or, when the record has none (a non-streamed reply, or a
// stream that produced no token), the end of the reply. Unknown when neither
// was observed.
func waitMS(r *uxrecord.Request) int32 {
	if ft := r.FirstTokenMS(); ft >= 0 {
		return ft
	}
	return r.EndMS
}

// anomalous is the sampler's rule for a record kept over the row budget: a
// non-2xx status, an outcome other than OK, a reply cut at the length limit,
// or a wait for the first output of at least slowMS.
func anomalous(r *uxrecord.Request, slowMS int32) bool {
	if r.Status < 200 || r.Status >= 300 || r.Outcome != uxrecord.OutcomeOK || r.FinishReason == "length" {
		return true
	}
	w := waitMS(r)
	return w >= 0 && w >= slowMS
}

// sampler decides which records get a request-log row. The rule, per
// wall-clock second of the writer's clock (all state resets each second):
//
//  1. The first budget records of the second are kept, Weight 1.
//  2. Past the budget, an anomalous record (see anomalous, keepSlowMS) is
//     always kept, Weight 1.
//  3. Past the budget, normal records are kept one in stride, Weight stride.
//     The stride starts at 2 and doubles every time another budget rows
//     have been kept at the current stride.
//
// So the kept normal records' weights add up to the normal records seen,
// short only by the ones skipped after the last kept row of the second
// (fewer than the final stride), and the rows a second costs grow with the
// logarithm of its traffic: budget * (1 + log2(normal/budget)) plus the
// anomalies.
type sampler struct {
	budget int

	second    int64 // the second the state below belongs to
	kept      int   // records kept in the budget phase (rule 1)
	stride    int32 // current stride of rule 3
	skipped   int32 // normal records seen since the last rule-3 keep
	strideRow int   // rows kept at the current stride
}

// keep reports whether r gets a row and the weight that row carries.
func (s *sampler) keep(r *uxrecord.Request, unixSecond int64) (bool, int32) {
	if unixSecond != s.second {
		*s = sampler{budget: s.budget, second: unixSecond, stride: 2}
	}
	if s.kept < s.budget {
		s.kept++
		return true, 1
	}
	if anomalous(r, keepSlowMS) {
		return true, 1
	}
	s.skipped++
	if s.skipped < s.stride {
		return false, 0
	}
	w := s.stride
	s.skipped = 0
	s.strideRow++
	if s.strideRow >= s.budget && s.stride < 1<<30 {
		s.stride *= 2
		s.strideRow = 0
	}
	return true, w
}
