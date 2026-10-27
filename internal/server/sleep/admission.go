package sleep

// GPUState is one GPU as the ledger sees it at the moment a wake is requested.
// AwakeBytes is what running, awake instances claim; SleepingBytes is the
// residue every sleeping instance on the card holds, the waking instance's own
// residue included. Occupants names the awake instances' deployments so a
// refusal can say who is in the way. HeldOutside is a card a process outside
// the platform holds: the ledger cannot see how much it uses.
type GPUState struct {
	Index         int
	MemoryTotal   int64
	AwakeBytes    int64
	SleepingBytes int64
	Occupants     []string
	HeldOutside   bool
}

// Claim is what one sleeping instance needs back on one GPU to run again: its
// whole-GPU utilization share or its memory limit plus reserve, exactly the
// figure the ledger charged for it while it was awake. ResidueBytes is what
// the ledger charges it on that GPU while it sleeps, handed back when it wakes.
type Claim struct {
	GPUIndex     int
	Bytes        int64
	ResidueBytes int64
}

// Shortfall is one GPU on which the wake does not fit: too little memory left,
// or (HeldOutside) a process outside the platform holds the card.
type Shortfall struct {
	GPUIndex    int      `json:"gpu_index"`
	NeedBytes   int64    `json:"need_bytes"`
	FreeBytes   int64    `json:"free_bytes"`
	Occupants   []string `json:"occupants,omitempty"`
	HeldOutside bool     `json:"held_outside,omitempty"`
}

// Verdict is the outcome of a wake admission check.
type Verdict struct {
	Shortfalls []Shortfall
}

// OK reports whether the wake fits on every GPU it needs.
func (v Verdict) OK() bool { return len(v.Shortfalls) == 0 }

// AdmitWake decides whether a sleeping instance can reclaim its claims. On each
// GPU the instance needs, the memory available to it is the total less what
// awake instances hold and less what the other sleepers keep resident; its own
// residue is handed back because waking replaces it rather than adding to it.
//
// A claim on a GPU the caller did not describe is refused rather than assumed
// to fit: an incomplete snapshot is the case that would otherwise wake an
// instance onto a card someone else is using. So is a claim on a card a
// process outside the platform holds, whose use the ledger cannot see.
func AdmitWake(gpus []GPUState, claims []Claim) Verdict {
	byIndex := make(map[int]GPUState, len(gpus))
	for _, g := range gpus {
		byIndex[g.Index] = g
	}
	var verdict Verdict
	claimsByGPU := make(map[int]int64, len(claims))
	residueByGPU := make(map[int]int64, len(claims))
	for _, claim := range claims {
		claimsByGPU[claim.GPUIndex] += claim.Bytes
		residueByGPU[claim.GPUIndex] += claim.ResidueBytes
	}
	for gpuIndex, need := range claimsByGPU {
		g, ok := byIndex[gpuIndex]
		if !ok {
			verdict.Shortfalls = append(verdict.Shortfalls, Shortfall{GPUIndex: gpuIndex, NeedBytes: need})
			continue
		}
		if g.HeldOutside {
			verdict.Shortfalls = append(verdict.Shortfalls, Shortfall{GPUIndex: g.Index, NeedBytes: need, HeldOutside: true})
			continue
		}
		// The ledger caps residue at what the card has left after awake claims,
		// so only the residue it actually charged can be handed back.
		own := residueByGPU[gpuIndex]
		if g.SleepingBytes < own {
			own = g.SleepingBytes
		}
		free := g.MemoryTotal - g.AwakeBytes - g.SleepingBytes + own
		if free < 0 {
			free = 0
		}
		if free < need {
			verdict.Shortfalls = append(verdict.Shortfalls, Shortfall{
				GPUIndex: g.Index, NeedBytes: need, FreeBytes: free, Occupants: g.Occupants,
			})
		}
	}
	return verdict
}
