package provider

import (
	"context"
	"fmt"
)

// InstanceDetail is the provider's own description of one instance, read
// from its detail endpoint (InstanceGetter). Some providers list a running
// instance without its connection data (RunPod's pod list carries neither the
// direct SSH endpoint nor the port mappings, only the single-pod read does),
// so a listing alone cannot say where an instance is reached now. It returns
// ErrUnsupported for a provider whose listing is complete.
func (s *Service) InstanceDetail(ctx context.Context, providerID uint, externalID string) (Instance, error) {
	p, err := s.store.GetProvider(providerID)
	if err != nil {
		return Instance{}, err
	}
	drv, ok := s.driver(p.Kind)
	if !ok {
		return Instance{}, fmt.Errorf("driver %s not registered", p.Kind)
	}
	getter, ok := drv.(InstanceGetter)
	if !ok {
		return Instance{}, ErrUnsupported
	}
	creds, err := s.creds(p)
	if err != nil {
		return Instance{}, err
	}
	return getter.GetInstance(ctx, creds, externalID)
}
