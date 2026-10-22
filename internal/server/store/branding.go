package store

import "encoding/json"

// Branding is how the console presents itself: the product name beside the
// logo, the header title, the sign-in page copy and the logo image. An empty
// field means the built-in default, which the UI renders in the viewer's
// language, so a missing row is the stock look.
type Branding struct {
	// ProductName replaces "GPUWorks" beside the logo, on the sign-in page and
	// in the browser tab title.
	ProductName string `json:"product_name,omitempty"`
	// ConsoleTitle replaces the header title, used verbatim.
	ConsoleTitle string `json:"console_title,omitempty"`
	// LoginDescription replaces the sign-in page's product blurb.
	LoginDescription string `json:"login_description,omitempty"`
	// Logo is a base64 data: URL of a PNG, JPEG, WebP or SVG image that
	// replaces the built-in mark.
	Logo string `json:"logo,omitempty"`
}

// Branding returns the current branding; a missing or corrupt value is the
// default (all fields empty).
func (s *Store) Branding() Branding {
	var b Branding
	if raw := s.GetSetting(SettingBranding); raw != "" {
		if err := json.Unmarshal([]byte(raw), &b); err != nil {
			return Branding{}
		}
	}
	return b
}

// SetBranding replaces the branding.
func (s *Store) SetBranding(b Branding) error {
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	return s.SetSetting(SettingBranding, string(raw))
}
