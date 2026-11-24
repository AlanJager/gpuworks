// Package storeplane supplies the inference gateway from this process's own
// store and event bus: the same reads and writes, at the same moments, the
// gateway made when it held the store itself.
package storeplane

import (
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/proxy/plane"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Source reads and writes the store for the gateway. The lookups and the
// report are the store's own methods.
type Source struct {
	*store.Store
}

// New returns the gateway's source over st, and no source for no store: the
// gateway then serves nothing and reads nothing.
func New(st *store.Store) plane.Source {
	if st == nil {
		return nil
	}
	return &Source{Store: st}
}

// Snapshot reads the routing rows. The generation is read before the rows: a
// change committed while they are read moves it on, and the rows then read as
// possibly stale.
func (s *Source) Snapshot() (*plane.Snapshot, error) {
	snap := &plane.Snapshot{Generation: s.Generation(plane.TopicInstance) + s.Generation(plane.TopicDeployment) + 1}
	var err error
	if snap.Deployments, err = s.RoutingDeployments(); err != nil {
		return nil, err
	}
	if snap.Instances, err = s.RoutingInstances(); err != nil {
		return nil, err
	}
	if snap.NodeUUID, snap.NodeDriver, err = s.RoutingNodes(); err != nil {
		return nil, err
	}
	return snap, nil
}

// Changes forwards the bus's events of the given topics. Without a bus there
// are none, and the channel stays empty.
func (s *Source) Changes(buffer int, topics ...string) (<-chan plane.Change, func()) {
	out := make(chan plane.Change, buffer)
	if s.Bus == nil {
		return out, func() {}
	}
	sub := s.Bus.Subscribe(buffer, topics...)
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			select {
			case e := <-sub.C():
				select {
				case out <- plane.Change{Topic: e.Topic, Key: e.Key, Reason: e.Reason}:
				case <-done:
					return
				}
			case <-done:
				return
			}
		}
	}()
	return out, func() {
		close(done)
		<-finished
		sub.Close()
	}
}

// Generation counts the changes the bus announced on a topic.
func (s *Source) Generation(topic string) uint64 {
	if s.Bus == nil {
		return 0
	}
	return s.Bus.Generation(topic)
}

// Announces reports whether the store has a bus.
func (s *Source) Announces() bool { return s.Bus != nil }

// DeploymentContract resolves the contract of a deployment's model.
func (s *Source) DeploymentContract(deployment *store.Deployment) inferenceapi.Contract {
	return inferenceapi.ResolveDeploymentContract(s.Store, deployment)
}

// DecryptAIKey opens an external model configuration's stored key.
func (s *Source) DecryptAIKey(box *auth.Secretbox, sealed string) (string, error) {
	return store.DecryptAIKey(box, sealed)
}
