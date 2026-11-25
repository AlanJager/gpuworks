package dto

// ConsoleManifest is GET /api/v1/console/manifest: what the console shell
// shows beyond its own pages. Every value is decided by the server; the
// console renders it and computes nothing. A server that adds nothing answers
// its edition and three empty arrays.
type ConsoleManifest struct {
	// Edition names the server build for display only.
	Edition  string            `json:"edition"`
	Features []ConsoleFeature  `json:"features"`
	Nav      []NavContribution `json:"nav"`
	Banners  []ConsoleBanner   `json:"banners"`
	// Platform is the limit on the whole server; absent while nothing is
	// limited.
	Platform *ConsolePlatform `json:"platform,omitempty"`
}

// ConsolePlatform is a limit on the whole server.
type ConsolePlatform struct {
	// Access is read_only (pages show, changes are refused) or locked
	// (pages show, changes and inference are refused).
	Access string `json:"access"`
	// Link is the console page where an administrator lifts the limit
	// (absent for anyone else), and
	// Action what the control that opens it says, rendered from its code;
	// absent leaves the console's own wording.
	Link   string          `json:"link,omitempty"`
	Action *ProductMessage `json:"action,omitempty"`
	// Message explains the limit, rendered from its code.
	Message ProductMessage `json:"message"`
}

// ConsoleFeature is one optional feature's availability, sorted by key.
type ConsoleFeature struct {
	Key string `json:"key"`
	// Access is full, read_only, locked or hidden. The console hides a
	// hidden feature, shows a locked one with Link, and offers no writes on
	// a read-only one.
	Access string `json:"access"`
	// Presentation is promote (shown when unavailable) or hidden.
	Presentation string `json:"presentation"`
	// Link is where the console sends a user about an unavailable feature.
	Link string `json:"link,omitempty"`
	// Message explains the current access, rendered from its code.
	Message *ProductMessage `json:"message,omitempty"`
	Limits  []FeatureLimit  `json:"limits"`
}

// FeatureLimit is one quantity a feature is available up to; a nil Max is
// unlimited.
type FeatureLimit struct {
	Dim string `json:"dim"`
	Max *int64 `json:"max"`
}

// NavContribution is a menu entry an extension adds to a console menu group.
type NavContribution struct {
	ID string `json:"id"`
	// Group is the id of the menu group the entry joins.
	Group string `json:"group"`
	// LabelKey is the i18n key of the entry's label.
	LabelKey string `json:"label_key"`
	Icon     string `json:"icon,omitempty"`
	// Route is the console path the entry opens.
	Route string `json:"route"`
	// Feature names the feature whose access the entry follows; empty for
	// an entry every server with the extension shows.
	Feature string `json:"feature,omitempty"`
}

// ConsoleBanner is a notice shown above every console page.
type ConsoleBanner struct {
	ID string `json:"id"`
	// Severity is info, warning or danger.
	Severity string         `json:"severity"`
	Message  ProductMessage `json:"message"`
	Link     string         `json:"link,omitempty"`
}
