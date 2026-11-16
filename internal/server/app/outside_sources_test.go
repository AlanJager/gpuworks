package app

import (
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/distribution"
)

// The console's channel client (update checks, the notice bell, the usage
// report and catalog updates all share it) is the edition's profile over the
// server's own outbound transport.
func TestOutsideSourcesUseTheEditionsProfile(t *testing.T) {
	application, _ := newTestApp(t, "gpuworks-test-password-2026")
	t.Cleanup(func() { _ = application.Store().Close() })
	privateHosts := func() []string { return nil }

	community := outsideSources(application.Store(), privateHosts, distribution.Profile{}).Channel
	if community.Edition != distribution.EditionCommunity || !slices.Equal(community.Sources, distribution.DefaultSources) || !community.Published ||
		community.HTTP == nil || community.HTTP.Transport == nil {
		t.Fatalf("community channel = %+v", community)
	}

	keys := distribution.Keys{"acme-1": distribution.TrustedKeys()["gpuworks-dist-1"]}
	acme := outsideSources(application.Store(), privateHosts, distribution.Profile{Edition: "acme", Sources: []string{"https://acme.example"}, Keys: keys}).Channel
	if acme.Edition != "acme" || !slices.Equal(acme.Sources, []string{"https://acme.example"}) || !acme.Published || len(acme.Keys) != 1 ||
		acme.HTTP == nil || acme.HTTP.Transport == nil {
		t.Fatalf("edition channel = %+v", acme)
	}
}
