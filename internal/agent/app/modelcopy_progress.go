package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

const copyStallTimeout = 2 * time.Minute

// copyProgress observes the worker without doing filesystem I/O. A blocked
// syscall therefore cannot suppress its progress/stall reports.
type copyProgress struct {
	mu              sync.Mutex
	done            atomic.Int64
	transferred     atomic.Int64
	waiting         atomic.Int64
	observation     proto.ModelPreparation
	total           int64
	lastDone        int64
	lastTransferred int64
	lastProgress    time.Time
	lastSample      time.Time
	lastRate        int64
	stableSamples   int
	report          func(proto.ModelFileStatus)
}

func newCopyProgress(report func(proto.ModelFileStatus)) *copyProgress {
	return &copyProgress{report: report}
}

func copyAttemptID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(id[:])
}

func (p *copyProgress) begin(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done.Store(0)
	p.transferred.Store(0)
	p.total, p.lastDone, p.lastTransferred = 0, 0, 0
	p.lastProgress, p.lastSample = now, now
	p.lastRate, p.stableSamples = 0, 0
	p.observation = proto.ModelPreparation{Attempt: copyAttemptID(), Phase: proto.ModelPhaseChecking}
	p.emitLocked(now)
}

func (p *copyProgress) phase(phase string, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	p.observation.Phase = phase
	p.observation.TotalKnown = true
	p.total = total
	p.lastProgress = now
	p.emitLocked(now)
}

func (p *copyProgress) sample(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.emitLocked(now)
}

func (p *copyProgress) emitLocked(now time.Time) {
	done, transferred := p.done.Load(), p.transferred.Load()
	if done != p.lastDone || p.waiting.Load() > 0 {
		p.lastProgress = now
	}
	idle := now.Sub(p.lastProgress)
	observation := p.observation
	observation.IdleSeconds = max(0, int64(idle/time.Second))
	observation.Stalled = idle >= copyStallTimeout
	var rate int64
	if elapsed := now.Sub(p.lastSample); elapsed > 0 && transferred >= p.lastTransferred {
		rate = int64(float64(transferred-p.lastTransferred) / elapsed.Seconds())
	}
	if observation.Phase != proto.ModelPhaseCopying || observation.Stalled {
		rate = 0
	}
	if rate > 0 && p.lastRate > 0 && rate >= p.lastRate/2 && rate <= p.lastRate*2 {
		p.stableSamples++
	} else {
		p.stableSamples = 0
	}
	observation.RateStable = p.stableSamples >= 2
	p.lastRate = rate
	p.lastDone, p.lastTransferred, p.lastSample = done, transferred, now
	p.report(proto.ModelFileStatus{State: proto.MFDownloading, BytesDone: done, BytesTotal: p.total,
		TransferBytesDone: transferred, RateBytesPerS: rate, Preparation: &observation})
}

func (p *copyProgress) monitor(ctx context.Context) func() {
	stop, ended := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ended)
		ticker := time.NewTicker(statusProgressInterval)
		defer ticker.Stop()
		for {
			select {
			case now := <-ticker.C:
				p.sample(now)
			case <-ctx.Done():
				return
			case <-stop:
				return
			}
		}
	}()
	return func() { close(stop); <-ended }
}
