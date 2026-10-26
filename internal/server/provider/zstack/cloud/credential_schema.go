package cloud

import (
	"embed"
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/pkg/localization"
)

//go:embed locales/*.json
var credentialLocaleFiles embed.FS

type credentialText struct {
	Label       string `json:"label"`
	Placeholder string `json:"placeholder"`
}

var credentialLocales = func() map[string]map[string]credentialText {
	locales, err := localization.LoadJSONFiles[map[string]credentialText](credentialLocaleFiles, "locales/%s.json", []string{localization.English, localization.Chinese})
	if err != nil {
		panic(fmt.Sprintf("load Cloud credential locales: %v", err))
	}
	return locales
}()

func (d *Driver) CredentialFields() []provider.CredentialField {
	fields := []provider.CredentialField{
		{Name: "endpoint", Required: true, DefaultTemplate: "http://MANAGEMENT_NODE_IP:8080/zstack"},
		{Name: "public_key", Required: true},
		{Name: "private_key", Required: true, Secret: true},
	}
	for i := range fields {
		field := &fields[i]
		field.Label, field.Placeholder = localization.Text{}, localization.Text{}
		for language, entries := range credentialLocales {
			entry := entries[field.Name]
			field.Label[language], field.Placeholder[language] = entry.Label, entry.Placeholder
		}
	}
	return fields
}

var _ provider.CredentialSchemaProvider = (*Driver)(nil)
