package modelmeta

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

// The repository record: when the repository was created and last changed, and
// whose logo stands for it.
type repositoryMeta struct {
	CreatedAt *time.Time
	UpdatedAt *time.Time
	LogoURL   string
}

type huggingFaceRepoMeta struct {
	ID           string `json:"_id"`
	Author       string `json:"author"`
	CreatedAt    string `json:"createdAt"`
	LastModified string `json:"lastModified"`
}

type modelScopeRepoMeta struct {
	Data struct {
		// Avatar is the uploader's picture, frequently ModelScope's shared
		// placeholder; the owning organization's Avatar is the logo, the
		// counterpart of Hugging Face's author avatar.
		Avatar       string `json:"Avatar"`
		Organization *struct {
			Avatar string `json:"Avatar"`
		} `json:"Organization"`
		CreatedTime     int64 `json:"CreatedTime"`
		LastUpdatedTime int64 `json:"LastUpdatedTime"`
	} `json:"Data"`
}

func fetchRepositoryMeta(ctx context.Context, source, repo, endpoint string, client *http.Client) (*repositoryMeta, error) {
	var meta repositoryMeta
	base := strings.TrimRight(endpoint, "/")
	if base == "" {
		base = modelrepo.BaseURL(source, "")
	}
	if source == modelrepo.SourceMS {
		body, err := getWith(ctx, client, base+"/api/v1/models/"+repo)
		if err != nil {
			return nil, err
		}
		var raw modelScopeRepoMeta
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, err
		}
		meta.LogoURL = raw.Data.Avatar
		if org := raw.Data.Organization; org != nil && strings.TrimSpace(org.Avatar) != "" {
			meta.LogoURL = org.Avatar
		}
		meta.CreatedAt = unixTime(raw.Data.CreatedTime)
		meta.UpdatedAt = unixTime(raw.Data.LastUpdatedTime)
		return &meta, nil
	}
	body, err := getWith(ctx, client, base+"/api/models/"+repo)
	if err != nil {
		return nil, err
	}
	var raw huggingFaceRepoMeta
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	meta.CreatedAt = parseTime(raw.CreatedAt)
	if meta.CreatedAt == nil {
		meta.CreatedAt = objectIDTime(raw.ID)
	}
	meta.UpdatedAt = parseTime(raw.LastModified)
	if raw.Author != "" {
		meta.LogoURL = base + "/avatars/" + raw.Author + ".png"
	}
	return &meta, nil
}

func objectIDTime(value string) *time.Time {
	if len(value) < 8 {
		return nil
	}
	seconds, err := strconv.ParseUint(value[:8], 16, 32)
	if err != nil || seconds == 0 {
		return nil
	}
	parsed := time.Unix(int64(seconds), 0).UTC()
	return &parsed
}

func parseTime(value string) *time.Time {
	if value == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339, time.RFC3339Nano} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return &parsed
		}
	}
	return nil
}

func unixTime(value int64) *time.Time {
	if value <= 0 {
		return nil
	}
	parsed := time.Unix(value, 0).UTC()
	return &parsed
}
