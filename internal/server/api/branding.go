package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// Branding limits. The logo travels inline with every sign-in page and console
// load, so it is capped well below anything a logo needs.
const (
	brandingProductNameMax      = 32
	brandingConsoleTitleMax     = 64
	brandingLoginDescriptionMax = 200
	brandingLogoMaxBytes        = 128 << 10
)

// brandingLogoTypes are the image types a logo may be. SVG is safe here: the
// UI only ever renders the logo through <img>, where scripts never run.
var brandingLogoTypes = []string{"image/png", "image/jpeg", "image/webp", "image/svg+xml"}

// getBranding is open to the sign-in page, which shows the product name, logo
// and blurb before anyone signs in; branding is not a secret.
func (a *apiHandlers) getBranding(w http.ResponseWriter, r *http.Request) {
	b := a.d.Store.Branding()
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, dto.Branding{
		ProductName:      b.ProductName,
		ConsoleTitle:     b.ConsoleTitle,
		LoginDescription: b.LoginDescription,
		Logo:             b.Logo,
	})
}

// patchBranding changes the fields the body names and leaves the rest; an
// empty string restores that field's default. The router admits
// administrators only.
func (a *apiHandlers) patchBranding(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProductName      *string `json:"product_name"`
		ConsoleTitle     *string `json:"console_title"`
		LoginDescription *string `json:"login_description"`
		Logo             *string `json:"logo"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*brandingLogoMaxBytes)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	b := a.d.Store.Branding()
	for _, f := range []struct {
		name      string
		in        *string
		out       *string
		max       int
		multiline bool
	}{
		{"product_name", req.ProductName, &b.ProductName, brandingProductNameMax, false},
		{"console_title", req.ConsoleTitle, &b.ConsoleTitle, brandingConsoleTitleMax, false},
		{"login_description", req.LoginDescription, &b.LoginDescription, brandingLoginDescriptionMax, true},
	} {
		if f.in == nil {
			continue
		}
		value := strings.TrimSpace(*f.in)
		if err := validBrandingText(value, f.max, f.multiline); err != nil {
			writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, f.name+": "+err.Error(), map[string]any{"field": f.name, "detail": f.name + ": " + err.Error()})
			return
		}
		*f.out = value
	}
	if req.Logo != nil {
		logo := strings.TrimSpace(*req.Logo)
		if err := validBrandingLogo(logo); err != nil {
			writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "logo: "+err.Error(), map[string]any{"field": "logo", "detail": "logo: " + err.Error()})
			return
		}
		b.Logo = logo
	}
	if err := a.d.Store.SetBranding(b); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	a.getBranding(w, r)
}

func validBrandingText(value string, max int, multiline bool) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("not valid UTF-8")
	}
	if n := utf8.RuneCountInString(value); n > max {
		return fmt.Errorf("at most %d characters, got %d", max, n)
	}
	for _, r := range value {
		if r == '\n' && multiline {
			continue
		}
		if unicode.IsControl(r) {
			return fmt.Errorf("contains a control character")
		}
	}
	return nil
}

// validBrandingLogo accepts "" (the built-in mark) or a base64 data: URL of
// one of brandingLogoTypes whose decoded image fits brandingLogoMaxBytes.
func validBrandingLogo(logo string) error {
	if logo == "" {
		return nil
	}
	rest, ok := strings.CutPrefix(logo, "data:")
	if !ok {
		return fmt.Errorf("must be a data: URL")
	}
	mediaType, data, ok := strings.Cut(rest, ";base64,")
	if !ok {
		return fmt.Errorf("must be a base64 data: URL")
	}
	known := false
	for _, t := range brandingLogoTypes {
		known = known || mediaType == t
	}
	if !known {
		return fmt.Errorf("image type %q is not one of %s", mediaType, strings.Join(brandingLogoTypes, ", "))
	}
	if base64.StdEncoding.DecodedLen(len(data)) > brandingLogoMaxBytes+2 {
		return fmt.Errorf("image is larger than %d KiB", brandingLogoMaxBytes>>10)
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return fmt.Errorf("invalid base64")
	}
	if len(raw) == 0 {
		return fmt.Errorf("image is empty")
	}
	if len(raw) > brandingLogoMaxBytes {
		return fmt.Errorf("image is larger than %d KiB", brandingLogoMaxBytes>>10)
	}
	return nil
}
