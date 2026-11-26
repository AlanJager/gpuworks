package manifest

import "testing"

func TestLegacyCatalogSelection(t *testing.T) {
	raw := []byte(`{"schema_version":4,"catalog_version":"release","series":[{"name":"runner","versions":[
 {"image_ref":"runner:manual","auto_select":"manual","registry_sources":["quay.io/team/runner:manual"]},
 {"image_ref":"runner:partial","arch":"multi","auto_select":"arm64"},
 {"image_ref":"runner:general","arch":"amd64"},
 {"image_ref":"runner:retired","deprecated":true}]}]}`)
	body, err := LegacyCatalog(raw)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if !m.IsLegacyProjection() || m.SHA256 == "" {
		t.Fatalf("unexpected legacy identity %v %s", m.SchemaVersion, m.CatalogVersion)
	}
	versions := m.Series[0].Versions
	for i, want := range []bool{true, false, false, true} {
		if versions[i].Deprecated != want || versions[i].AutoSelect != "" {
			t.Errorf("%s: deprecated=%t auto_select=%q", versions[i].ImageRef, versions[i].Deprecated, versions[i].AutoSelect)
		}
	}
	if versions[1].Arch != "multi" || len(versions[0].RegistrySources) != 1 || versions[0].RegistrySources[0] != "quay.io/team/runner:manual" {
		t.Fatal("lost multi-platform support or template pull sources")
	}
}
