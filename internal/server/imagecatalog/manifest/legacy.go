package manifest

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// A schema 3 reader cannot enforce per-architecture or manual selection.
// Its deprecated rows still resolve explicit references and their mirrors.
func LegacyCatalog(body []byte) ([]byte, error) {
	m, err := Parse(body)
	if err != nil || m.SchemaVersion != RunnerSchema {
		return body, err
	}
	m.SchemaVersion = 3
	for _, series := range m.Series {
		for i := range series.Versions {
			v := &series.Versions[i]
			if v.AutoSelect != "" {
				v.Deprecated = true
			}
			v.AutoSelect = ""
			v.RegistryDigest = ""
		}
	}
	data, err := json.Marshal(m.Series)
	if err != nil {
		return nil, err
	}
	m.CatalogVersion = fmt.Sprintf("schema3.%x", sha256.Sum256(data))
	m.Signature = ""
	m.SHA256 = ComputeSHA256(m)
	body, err = json.MarshalIndent(m, "", "  ")
	return append(body, '\n'), err
}
