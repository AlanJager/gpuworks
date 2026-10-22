package dto

// Branding is how the console presents itself (store.Branding). An empty field
// means the built-in default, which the client renders in its own language.
type Branding struct {
	ProductName      string `json:"product_name"`
	ConsoleTitle     string `json:"console_title"`
	LoginDescription string `json:"login_description"`
	// Logo is a base64 data: URL (PNG, JPEG, WebP or SVG), or empty for the
	// built-in mark.
	Logo string `json:"logo"`
}
