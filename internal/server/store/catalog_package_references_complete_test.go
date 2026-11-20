//go:build linux && dqlite

package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"gorm.io/gorm"
)

func TestCompleteCatalogPackageReferencesFailsOnAnUnreadableReference(t *testing.T) {
	st := newTestStore(t)
	first, second, third := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	model := InferenceModel{ModelID: "org/model", Source: "user", PackageDigest: second}
	if err := st.db.Create(&model).Error; err != nil {
		t.Fatal(err)
	}
	deployment := Deployment{Name: "d", CatalogSelectionSnapshot: `{"runtime_assets":[{"package_digest":"` + first + `"},{"package_digest":""}]}`}
	if err := st.db.Create(&deployment).Error; err != nil {
		t.Fatal(err)
	}
	instance := Instance{DeploymentID: deployment.ID, DesiredStateJSON: `{"launch":{"runtime_assets":[{"package_digest":"` + strings.ToUpper(third) + `"}]}}`}
	if err := st.db.Create(&instance).Error; err != nil {
		t.Fatal(err)
	}
	got, err := st.CompleteCatalogPackageReferences(t.Context())
	if err != nil || !reflect.DeepEqual(got, []string{first, second, third}) {
		t.Fatalf("references = %v, %v; want the three digests in order", got, err)
	}
	tolerant, err := st.ReferencedCatalogPackageDigests()
	if err != nil || !reflect.DeepEqual(tolerant, got) {
		t.Fatalf("the two reads disagree on valid references: %v, %v", tolerant, err)
	}

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if got, err := st.CompleteCatalogPackageReferences(cancelled); err == nil || got != nil {
		t.Fatalf("cancelled read returned %v, %v", got, err)
	}

	for name, damage := range map[string]func(tx *gorm.DB) error{
		"model digest": func(tx *gorm.DB) error {
			return tx.Model(&model).Update("package_digest", "private-invalid").Error
		},
		"deployment snapshot JSON": func(tx *gorm.DB) error {
			return tx.Model(&deployment).Update("template_snapshot", `{"runtime_assets":`).Error
		},
		"deployment asset digest": func(tx *gorm.DB) error {
			return tx.Model(&deployment).Update("template_snapshot", `{"runtime_assets":[{"package_digest":"short"}]}`).Error
		},
		"instance spec JSON": func(tx *gorm.DB) error {
			return tx.Model(&instance).Update("spec", `not json`).Error
		},
	} {
		t.Run(name, func(t *testing.T) {
			tx := st.db.Begin()
			defer tx.Rollback()
			if err := damage(tx); err != nil {
				t.Fatal(err)
			}
			got, err := (&Store{db: tx}).CompleteCatalogPackageReferences(t.Context())
			if err == nil || got != nil {
				t.Fatalf("damaged %s returned %v, %v; want an error and no list", name, got, err)
			}
			if strings.Contains(err.Error(), "private-invalid") {
				t.Fatalf("error repeats the stored value: %v", err)
			}
			if name == "model digest" && !errors.Is(err, errInvalidCatalogPackageReference) {
				t.Fatalf("invalid digest error = %v", err)
			}
		})
	}
}
