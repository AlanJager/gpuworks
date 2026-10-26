package store

import "gorm.io/gorm"

// addInstanceTranscriptionServed records whether the engine's OpenAPI
// document exposes transcription. Existing rows stay NULL (unknown).
func addInstanceTranscriptionServed(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Instance{}) || db.Migrator().HasColumn(&Instance{}, "TranscriptionServed") {
		return nil
	}
	return db.Migrator().AddColumn(&Instance{}, "TranscriptionServed")
}
