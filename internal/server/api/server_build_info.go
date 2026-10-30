package api

import (
	"regexp"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/distribution"
	"gpuworks.ai/gpuworks/internal/version"
)

// Complete semantic versions only: a release channel name or a version-like
// prefix does not establish that this is a stable release. The fourth capture
// is the prerelease identifier; numeric identifiers cannot have leading zeroes.
var publishedSemver = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-((?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
var fullBuildCommit = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)

// agentUpgradeBuildInfo never probes a distribution source. A confirmation
// remains available offline and uses only the update page's verified cache.
func (a *apiHandlers) agentUpgradeBuildInfo(target string) dto.ServerBuildInfo {
	fetched, err, _, _ := a.updates.TryCached(a.channelName())
	if err != nil {
		fetched = nil
	}
	return serverBuildInfo(target, fetched)
}

func serverBuildInfo(target string, fetched *distribution.Fetched) dto.ServerBuildInfo {
	info := dto.ServerBuildInfo{Version: target, Kind: "unknown"}
	if target != version.Version {
		return info
	}
	info.BuildChannel = version.Channel
	if version.Commit != "" && version.Commit != "none" {
		info.Commit = version.Commit
	}
	built, buildErr := time.Parse(time.RFC3339, version.BuildDate)
	if buildErr == nil && !built.IsZero() {
		info.BuiltAt = version.BuildDate
	}
	// This is immutable build metadata, not the user-selected update channel.
	// It describes a development/test build, not proof of a published nightly.
	if version.Channel == distribution.ChannelNightly {
		info.Kind = "development"
	}
	if fetched == nil || buildErr != nil || built.IsZero() || !fullBuildCommit.MatchString(version.Commit) {
		return info
	}
	release := fetched.Index.Server
	publishedBuild, err := time.Parse(time.RFC3339, release.BuiltAt)
	if release.ReleaseID != target || !strings.EqualFold(release.Commit, version.Commit) || err != nil || !built.Equal(publishedBuild) {
		return info
	}
	info.PublishedVersion = release.ReleaseID
	if !fetched.Index.PublishedAt.IsZero() {
		info.PublishedAt = fetched.Index.PublishedAt.UTC().Format(time.RFC3339Nano)
	}
	switch fetched.Index.Channel {
	case distribution.ChannelNightly:
		info.Kind = "prerelease"
	case distribution.ChannelRelease:
		info.Kind = "unknown"
		if parts := publishedSemver.FindStringSubmatch(release.ReleaseID); parts != nil {
			info.Kind = "stable"
			if parts[4] != "" {
				info.Kind = "prerelease"
			}
		}
	}
	return info
}
