package store

import (
	"errors"
	"fmt"
	"slices"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

// APIKeyScope is which deployments an API key may call. The zero value is
// every deployment; Restricted limits it to DeploymentIDs.
type APIKeyScope struct {
	Restricted    bool
	DeploymentIDs []uint
}

// UnknownDeploymentsError names the granted deployments that do not exist.
type UnknownDeploymentsError struct{ IDs []uint }

func (e *UnknownDeploymentsError) Error() string {
	return fmt.Sprintf("unknown deployments %v", e.IDs)
}

// ErrAPIKeyScopeEmpty refuses a restricted scope without any deployment: a
// key that can call nothing is never what was meant at creation or edit.
var ErrAPIKeyScopeEmpty = errors.New("a restricted API key needs at least one deployment")

// Allows says whether the key may call deployment id.
func (i APIKeyIdentity) Allows(id uint) bool {
	if !i.Restricted {
		return true
	}
	_, ok := i.Deployments[id]
	return ok
}

// CreateScopedAPIKey is CreateAPIKey limited to scope.
func (s *Store) CreateScopedAPIKey(name string, userID uint, scope APIKeyScope) (fullKey string, err error) {
	ids, err := normalizeAPIKeyScope(scope)
	if err != nil {
		return "", err
	}
	full, prefix := auth.NewAPIKey()
	hash, err := auth.HashPassword(full)
	if err != nil {
		return "", err
	}
	k := &APIKey{Name: name, Prefix: prefix, Hash: hash, UserID: userID, Restricted: scope.Restricted}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(k).Error; err != nil {
			return err
		}
		return grantAPIKeyDeployments(tx, k.ID, ids)
	})
	if err != nil {
		return "", err
	}
	s.apiKeys.changed()
	s.Bus.PublishChange("apikey", prefix, "created")
	return full, nil
}

// SetAPIKeyScope replaces a live key's scope. An unknown or revoked key is
// ErrNotFound. The gateway sees the new scope on the key's next request.
func (s *Store) SetAPIKeyScope(prefix string, scope APIKeyScope) error {
	ids, err := normalizeAPIKeyScope(scope)
	if err != nil {
		return err
	}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		var k APIKey
		if err := tx.Where("prefix = ? AND revoked_at IS NULL", prefix).First(&k).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if err := tx.Model(&APIKey{}).Where("id = ?", k.ID).Update("restricted", scope.Restricted).Error; err != nil {
			return err
		}
		if err := tx.Where("api_key_id = ?", k.ID).Delete(&APIKeyDeployment{}).Error; err != nil {
			return err
		}
		return grantAPIKeyDeployments(tx, k.ID, ids)
	})
	// Forget the cached scope even when the write failed: re-reading it is
	// always safe, trusting a possibly widened one is not.
	s.apiKeys.forget(prefix)
	if err == nil {
		s.Bus.PublishChange("apikey", prefix, "updated")
	}
	return err
}

// APIKeyScopeByID is the scope of key id, revoked or not, for a request sent
// on its behalf without its secret (a batch's, possibly long after the batch
// was created). An unknown key is ErrNotFound. Cached like ResolveAPIKey.
func (s *Store) APIKeyScopeByID(id uint) (APIKeyIdentity, error) {
	if cached, ok := s.apiKeys.lookupScope(id); ok {
		return cached, nil
	}
	generation := s.apiKeys.snapshot()
	var k APIKey
	if err := s.db.Select("id", "user_id", "prefix", "restricted").First(&k, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return APIKeyIdentity{}, ErrNotFound
		}
		return APIKeyIdentity{}, err
	}
	identity := APIKeyIdentity{KeyID: k.ID, UserID: k.UserID, Restricted: k.Restricted}
	if k.Restricted {
		set, err := apiKeyDeploymentSet(s.db, k.ID)
		if err != nil {
			return APIKeyIdentity{}, err
		}
		identity.Deployments = set
	}
	s.apiKeys.rememberScope(generation, identity, k.Prefix)
	return identity, nil
}

// APIKeyGrant is one deployment a restricted key may call, with the names
// the key list shows for it.
type APIKeyGrant struct {
	APIKeyID     uint
	DeploymentID uint
	Name         string
	ServedName   string
}

// APIKeyGrants returns every key's grants by key ID, in deployment ID order.
// Keys without grants are absent.
func (s *Store) APIKeyGrants() (map[uint][]APIKeyGrant, error) {
	var rows []APIKeyGrant
	if err := s.reader().Table("api_key_deployments g").
		Select("g.api_key_id, g.deployment_id, d.name, d.served_name").
		Joins("JOIN deployments d ON d.id = g.deployment_id").
		Order("g.api_key_id, g.deployment_id").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[uint][]APIKeyGrant)
	for _, r := range rows {
		out[r.APIKeyID] = append(out[r.APIKeyID], r)
	}
	return out, nil
}

// normalizeAPIKeyScope returns the scope's deployment IDs sorted and without
// duplicates, refusing a restricted scope without any. An unrestricted scope
// grants nothing explicitly, whatever IDs it carries.
func normalizeAPIKeyScope(scope APIKeyScope) ([]uint, error) {
	if !scope.Restricted {
		return nil, nil
	}
	ids := slices.Clone(scope.DeploymentIDs)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) == 0 || ids[0] == 0 {
		return nil, ErrAPIKeyScopeEmpty
	}
	return ids, nil
}

// grantAPIKeyDeployments inserts the grants after checking, in the same
// transaction, that every deployment exists.
func grantAPIKeyDeployments(tx *gorm.DB, keyID uint, ids []uint) error {
	if len(ids) == 0 {
		return nil
	}
	var found []uint
	if err := tx.Model(&Deployment{}).Where("id IN ?", ids).Pluck("id", &found).Error; err != nil {
		return err
	}
	if len(found) != len(ids) {
		var missing []uint
		for _, id := range ids {
			if !slices.Contains(found, id) {
				missing = append(missing, id)
			}
		}
		return &UnknownDeploymentsError{IDs: missing}
	}
	rows := make([]APIKeyDeployment, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, APIKeyDeployment{APIKeyID: keyID, DeploymentID: id})
	}
	return tx.Create(&rows).Error
}

// apiKeyDeploymentSet reads one key's grants for the gateway.
func apiKeyDeploymentSet(db *gorm.DB, keyID uint) (map[uint]struct{}, error) {
	var ids []uint
	if err := db.Model(&APIKeyDeployment{}).Where("api_key_id = ?", keyID).Pluck("deployment_id", &ids).Error; err != nil {
		return nil, err
	}
	set := make(map[uint]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return set, nil
}

// dropDeploymentGrants removes a deployment's grants inside its delete
// transaction and returns the prefixes of the keys that had one, whose cached
// scope must then be forgotten.
func dropDeploymentGrants(tx *gorm.DB, deploymentID uint) ([]string, error) {
	var prefixes []string
	if err := tx.Model(&APIKey{}).
		Joins("JOIN api_key_deployments g ON g.api_key_id = api_keys.id").
		Where("g.deployment_id = ?", deploymentID).
		Pluck("api_keys.prefix", &prefixes).Error; err != nil {
		return nil, err
	}
	if err := tx.Where("deployment_id = ?", deploymentID).Delete(&APIKeyDeployment{}).Error; err != nil {
		return nil, err
	}
	return prefixes, nil
}
