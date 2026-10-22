package store

import "time"

// AIConfig is an OpenAI-compatible LLM endpoint for the AI helper (plan §14).
// The product is 100% usable without any configured — AI features simply hide.
type AIConfig struct {
	ID        uint   `gorm:"primaryKey"`
	Name      string `gorm:"size:64"`
	Preset    string `gorm:"size:32"`  // deepseek|bailian|siliconflow|openrouter|custom
	BaseURL   string `gorm:"size:255"` // e.g. https://api.deepseek.com/v1
	APIKey    string `gorm:"size:512"` // enc:v1:<base64>; legacy plaintext is migrated on read
	Model     string `gorm:"size:64"`
	IsDefault bool
	CreatedAt time.Time
	UpdatedAt time.Time
}
