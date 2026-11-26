package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const legacyVersionPrefix = "schema3."

// IsLegacyProjection identifies the lossy catalog emitted for older readers.
func (m Manifest) IsLegacyProjection() bool {
	suffix, ok := strings.CutPrefix(m.CatalogVersion, legacyVersionPrefix)
	if m.SchemaVersion != 3 || !ok || len(suffix) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}

// A schema 3 reader selects a partially eligible multi-platform image as multi.
// Only manual releases need to be hidden from its automatic selector.
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
			if v.AutoSelect == "manual" {
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
	m.CatalogVersion = fmt.Sprintf(legacyVersionPrefix+"%x", sha256.Sum256(data))
	m.Signature = ""
	m.SHA256 = ComputeSHA256(m)
	body, err = json.MarshalIndent(m, "", "  ")
	return append(body, '\n'), err
}
