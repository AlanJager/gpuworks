package rollup

import (
	"sync"
	"time"
)

// GPUKey is one row of the GPU utilization: an hour and a card, known by its
// node and device index and, when the prober reports one, its UUID.
type GPUKey struct {
	HourMS   int64
	NodeUUID string
	GPUIndex int
	GPUUUID  string
}

// GPU is a card's samples of an hour (one every 10 s): sums and counts for
// the means, maxima for the peaks.
type GPU struct {
	UtilSum, UtilMax       float64
	UtilN                  int64
	MemUsedSum, MemUsedMax float64
	MemUsedN               int64
	// MemTotal is the card's memory, the largest seen.
	MemTotal         float64
	PowerSum         float64
	PowerN           int64
	TempSum, TempMax float64
	TempN            int64
}

// Add adds o to g.
func (g *GPU) Add(o GPU) {
	g.UtilSum += o.UtilSum
	g.UtilN += o.UtilN
	g.UtilMax = max(g.UtilMax, o.UtilMax)
	g.MemUsedSum += o.MemUsedSum
	g.MemUsedN += o.MemUsedN
	g.MemUsedMax = max(g.MemUsedMax, o.MemUsedMax)
	g.MemTotal = max(g.MemTotal, o.MemTotal)
	g.PowerSum += o.PowerSum
	g.PowerN += o.PowerN
	g.TempSum += o.TempSum
	g.TempN += o.TempN
	g.TempMax = max(g.TempMax, o.TempMax)
}

// GPUMetric is a GPU sample the utilization counts.
type GPUMetric uint8

const (
	GPUUtilization GPUMetric = iota + 1
	GPUMemoryUsed
	GPUMemoryTotal
	GPUPower
	GPUTemperature
)

// GPUs accumulates GPU samples. The metrics ingest is its one writer;
// Observe and Take may run concurrently.
type GPUs struct {
	mu   sync.Mutex
	rows map[GPUKey]*GPU
}

// NewGPUs returns an empty accumulator.
func NewGPUs() *GPUs { return &GPUs{rows: map[GPUKey]*GPU{}} }

// GPUSample is one GPU sample of an ingest batch.
type GPUSample struct {
	Key    GPUKey // HourMS is filled from TimeMS
	Metric GPUMetric
	Value  float64
	TimeMS int64
}

// Observe adds a batch of samples under one lock.
func (g *GPUs) Observe(samples []GPUSample) {
	if len(samples) == 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, s := range samples {
		key := s.Key
		key.HourMS = HourOfMS(s.TimeMS)
		row := g.rows[key]
		if row == nil {
			row = &GPU{}
			g.rows[key] = row
		}
		v := s.Value
		switch s.Metric {
		case GPUUtilization:
			row.UtilSum += v
			row.UtilN++
			row.UtilMax = max(row.UtilMax, v)
		case GPUMemoryUsed:
			row.MemUsedSum += v
			row.MemUsedN++
			row.MemUsedMax = max(row.MemUsedMax, v)
		case GPUMemoryTotal:
			row.MemTotal = max(row.MemTotal, v)
		case GPUPower:
			row.PowerSum += v
			row.PowerN++
		case GPUTemperature:
			row.TempSum += v
			row.TempN++
			row.TempMax = max(row.TempMax, v)
		}
	}
}

// Take returns and clears what accumulated.
func (g *GPUs) Take() map[GPUKey]GPU {
	g.mu.Lock()
	rows := g.rows
	g.rows = make(map[GPUKey]*GPU, len(rows))
	g.mu.Unlock()
	out := make(map[GPUKey]GPU, len(rows))
	for k, v := range rows {
		out[k] = *v
	}
	return out
}

// GiveBack merges a delta a flush could not write back in.
func (g *GPUs) GiveBack(delta map[GPUKey]GPU) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, v := range delta {
		if cur := g.rows[k]; cur != nil {
			cur.Add(v)
		} else {
			v := v
			g.rows[k] = &v
		}
	}
}

// GPUTimeKey is one row of the card time: an hour, a deployment and the
// model of the cards it held.
type GPUTimeKey struct {
	HourMS       int64
	DeploymentID uint
	GPUModel     string
}

// GPUTime is the cards a deployment held over an hour.
type GPUTime struct {
	// GPUSeconds weighs a dGPU card by its share of the card's memory;
	// WholeGPUSeconds counts every held card whole.
	GPUSeconds      float64
	WholeGPUSeconds float64
	// InstanceSeconds over the hour is the mean number of replicas;
	// MaxInstances the most seen in one sample.
	InstanceSeconds float64
	MaxInstances    int64
}

// Add adds o to t; the maximum is a maximum.
func (t *GPUTime) Add(o GPUTime) {
	t.GPUSeconds += o.GPUSeconds
	t.WholeGPUSeconds += o.WholeGPUSeconds
	t.InstanceSeconds += o.InstanceSeconds
	t.MaxInstances = max(t.MaxInstances, o.MaxInstances)
}

// CapacityKey is one row of the cluster's card capacity: an hour and a card
// model.
type CapacityKey struct {
	HourMS   int64
	GPUModel string
}

// Capacity is the cards of a model the cluster had over an hour: on online
// nodes, and of those the ones that can take work.
type Capacity struct {
	TotalGPUSeconds       float64
	SchedulableGPUSeconds float64
}

// Add adds o to c.
func (c *Capacity) Add(o Capacity) {
	c.TotalGPUSeconds += o.TotalGPUSeconds
	c.SchedulableGPUSeconds += o.SchedulableGPUSeconds
}

// Holding is an instance that held cards at a sample.
type Holding struct {
	DeploymentID uint
	Cards        []HeldCard
}

// HeldCard is one card an instance held: its model and the instance's
// share of it (1 for a whole card, the memory share for a dGPU).
type HeldCard struct {
	Model string
	Share float64
}

// Card is a card the cluster had at a sample.
type Card struct {
	Model       string
	Schedulable bool
}

// GPUTimes accumulates the card time sampled once a minute.
type GPUTimes struct {
	mu       sync.Mutex
	time     map[GPUTimeKey]*GPUTime
	capacity map[CapacityKey]*Capacity
}

// NewGPUTimes returns an empty accumulator.
func NewGPUTimes() *GPUTimes {
	return &GPUTimes{time: map[GPUTimeKey]*GPUTime{}, capacity: map[CapacityKey]*Capacity{}}
}

// Sample counts what was held at `at` as held for interval. An instance
// counts once, under the model of its first card.
func (g *GPUTimes) Sample(at time.Time, interval time.Duration, holdings []Holding, cards []Card) {
	hour := HourMS(at)
	seconds := interval.Seconds()
	type depModel struct {
		dep   uint
		model string
	}
	instances := map[depModel]int64{}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, h := range holdings {
		if len(h.Cards) == 0 {
			continue
		}
		for _, c := range h.Cards {
			row := g.timeRow(GPUTimeKey{HourMS: hour, DeploymentID: h.DeploymentID, GPUModel: c.Model})
			row.GPUSeconds += c.Share * seconds
			row.WholeGPUSeconds += seconds
		}
		first := depModel{h.DeploymentID, h.Cards[0].Model}
		g.timeRow(GPUTimeKey{HourMS: hour, DeploymentID: first.dep, GPUModel: first.model}).InstanceSeconds += seconds
		instances[first]++
	}
	for k, n := range instances {
		row := g.timeRow(GPUTimeKey{HourMS: hour, DeploymentID: k.dep, GPUModel: k.model})
		row.MaxInstances = max(row.MaxInstances, n)
	}
	for _, c := range cards {
		key := CapacityKey{HourMS: hour, GPUModel: c.Model}
		row := g.capacity[key]
		if row == nil {
			row = &Capacity{}
			g.capacity[key] = row
		}
		row.TotalGPUSeconds += seconds
		if c.Schedulable {
			row.SchedulableGPUSeconds += seconds
		}
	}
}

func (g *GPUTimes) timeRow(key GPUTimeKey) *GPUTime {
	row := g.time[key]
	if row == nil {
		row = &GPUTime{}
		g.time[key] = row
	}
	return row
}

// Take returns and clears what accumulated.
func (g *GPUTimes) Take() (map[GPUTimeKey]GPUTime, map[CapacityKey]Capacity) {
	g.mu.Lock()
	times, capacity := g.time, g.capacity
	g.time, g.capacity = make(map[GPUTimeKey]*GPUTime, len(times)), make(map[CapacityKey]*Capacity, len(capacity))
	g.mu.Unlock()
	outT := make(map[GPUTimeKey]GPUTime, len(times))
	for k, v := range times {
		outT[k] = *v
	}
	outC := make(map[CapacityKey]Capacity, len(capacity))
	for k, v := range capacity {
		outC[k] = *v
	}
	return outT, outC
}

// GiveBack merges a delta a flush could not write back in.
func (g *GPUTimes) GiveBack(times map[GPUTimeKey]GPUTime, capacity map[CapacityKey]Capacity) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, v := range times {
		g.timeRow(k).Add(v)
	}
	for k, v := range capacity {
		row := g.capacity[k]
		if row == nil {
			row = &Capacity{}
			g.capacity[k] = row
		}
		row.Add(v)
	}
}
