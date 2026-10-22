package loadgen

import (
	"math"
	"math/rand"
	"time"
)

// Arrival is an open-loop arrival process: Rate sessions per second whose
// inter-arrival gaps follow a gamma distribution with shape Burstiness and
// mean 1/Rate. Burstiness 1 is a Poisson process; below 1 arrivals cluster,
// above 1 they spread out evenly. This is the arrival model of vLLM's bench
// serve (--request-rate, --burstiness).
type Arrival struct {
	Rate       float64
	Burstiness float64
	// Seed makes the schedule reproducible; 0 draws a random one.
	Seed int64
}

// Gaps returns the n-1 waits between n arrivals.
func (a Arrival) Gaps(n int) []time.Duration {
	if n <= 1 || a.Rate <= 0 {
		return nil
	}
	shape := a.Burstiness
	if shape <= 0 {
		shape = 1
	}
	seed := a.Seed
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	r := rand.New(rand.NewSource(seed))
	scale := 1 / (a.Rate * shape)
	out := make([]time.Duration, n-1)
	for i := range out {
		out[i] = time.Duration(gamma(r, shape) * scale * float64(time.Second))
	}
	return out
}

// gamma draws from Gamma(shape, 1) with Marsaglia and Tsang's method; shapes
// below 1 use the boost Gamma(shape+1) · U^(1/shape).
func gamma(r *rand.Rand, shape float64) float64 {
	if shape < 1 {
		u := r.Float64()
		for u == 0 {
			u = r.Float64()
		}
		return gamma(r, shape+1) * math.Pow(u, 1/shape)
	}
	d := shape - 1.0/3
	c := 1 / math.Sqrt(9*d)
	for {
		x := r.NormFloat64()
		v := 1 + c*x
		if v <= 0 {
			continue
		}
		v = v * v * v
		u := r.Float64()
		if u < 1-0.0331*x*x*x*x {
			return d * v
		}
		if u > 0 && math.Log(u) < 0.5*x*x+d*(1-v+math.Log(v)) {
			return d * v
		}
	}
}
