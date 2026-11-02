package modelmeta

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
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
	meta.LogoURL = huggingFaceAvatar(ctx, client, base, raw.Author)
	return &meta, nil
}

// huggingFaceAvatar asks the hub for the author's uploaded picture, as an
// organization and then as a user. The hub answers every name (including
// unknown ones) at /avatars/<name>.svg with a generated gradient, and points an
// account without an upload at such a .svg too; neither is a logo.
func huggingFaceAvatar(ctx context.Context, client *http.Client, base, author string) string {
	author = strings.TrimSpace(author)
	if author == "" {
		return ""
	}
	for _, kind := range []string{"organizations", "users"} {
		body, err := getWith(ctx, client, base+"/api/"+kind+"/"+url.PathEscape(author)+"/avatar")
		if err != nil {
			continue
		}
		var raw struct {
			AvatarURL string `json:"avatarUrl"`
		}
		if json.Unmarshal(body, &raw) != nil || strings.TrimSpace(raw.AvatarURL) == "" {
			continue
		}
		parsed, err := url.Parse(strings.TrimSpace(raw.AvatarURL))
		if err != nil || strings.HasSuffix(strings.ToLower(parsed.Path), ".svg") {
			return ""
		}
		if !parsed.IsAbs() {
			if parsed, err = url.Parse(base + "/" + strings.TrimPrefix(parsed.String(), "/")); err != nil {
				return ""
			}
		}
		return parsed.String()
	}
	return ""
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
