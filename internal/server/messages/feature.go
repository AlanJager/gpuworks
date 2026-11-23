package messages

// Refusals of a feature that a separately built server adds
// (internal/server/featuregate). Params name the feature ({feature}); a
// limit refusal also names the dimension, its maximum and what is used.
const (
	// ErrorFeatureUnavailable: the feature is not available on this server.
	ErrorFeatureUnavailable = "FEATURE_UNAVAILABLE"
	// ErrorFeatureReadOnly: the feature is read-only; reads work, writes
	// are refused.
	ErrorFeatureReadOnly = "FEATURE_READ_ONLY"
	// ErrorFeatureProviderAbsent: the feature is declared, but nothing on
	// this server answers whether it is available, so it is refused.
	ErrorFeatureProviderAbsent = "FEATURE_PROVIDER_ABSENT"
	// ErrorFeatureLimitReached: the change would take the feature past the
	// quantity it is available up to ({dim}, {max}, {used}).
	ErrorFeatureLimitReached = "FEATURE_LIMIT_REACHED"
)

// Refusals of a server that is limited as a whole (api.PlatformLimiter).
const (
	// ErrorPlatformLocked: the server serves only signing in, health and
	// what lifts the limit.
	ErrorPlatformLocked = "PLATFORM_LOCKED"
	// ErrorPlatformReadOnly: the server serves reads and the data plane;
	// changes are refused.
	ErrorPlatformReadOnly = "PLATFORM_READ_ONLY"
)
