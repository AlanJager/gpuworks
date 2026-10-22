package compshare

import (
	"context"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

type customImageRecord struct {
	CompShareImageID string `json:"CompShareImageId"`
	Name             string `json:"Name"`
	Status           string `json:"Status"`
	CreateTime       int64  `json:"CreateTime"`
}

// CustomImageRequest describes a CompShare custom image created from an
// existing VM. The source VM is explicit so image preparation tools cannot
// accidentally snapshot an unrelated account resource.
type CustomImageRequest struct {
	InstanceID  string
	Name        string
	Description string
	Region      string
	Zone        string
}

// CustomImage is lifecycle metadata returned by the custom-image list API.
type CustomImage struct {
	ID         string
	Name       string
	Status     string
	CreateTime int64
}

// CreateCustomImage starts an asynchronous custom-image snapshot.
func (d *Driver) CreateCustomImage(ctx context.Context, creds provider.Credentials, req CustomImageRequest) (string, error) {
	instanceID := strings.TrimSpace(req.InstanceID)
	if instanceID == "" {
		return "", fmt.Errorf("compshare: source instance id is required")
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return "", fmt.Errorf("compshare: custom image name is required")
	}
	params := map[string]string{
		"Action":  "CreateCompShareCustomImage",
		"Region":  firstNonEmpty(req.Region, defaultRegion),
		"Zone":    firstNonEmpty(req.Zone, defaultZone),
		"UHostId": instanceID,
		"Name":    name,
	}
	if description := strings.TrimSpace(req.Description); description != "" {
		params["Description"] = description
	}
	resp, err := d.call(ctx, creds, params)
	if err != nil {
		return "", fmt.Errorf("compshare: create custom image: %w", err)
	}
	if strings.TrimSpace(resp.CompShareImageID) == "" {
		return "", fmt.Errorf("compshare: create custom image returned no image id")
	}
	return strings.TrimSpace(resp.CompShareImageID), nil
}

// CustomImageProgress returns the provider-reported snapshot completion
// percentage. Callers own the polling policy and context deadline.
func (d *Driver) CustomImageProgress(ctx context.Context, creds provider.Credentials, imageID, region, zone string) (float64, error) {
	imageID = strings.TrimSpace(imageID)
	if imageID == "" {
		return 0, fmt.Errorf("compshare: custom image id is required")
	}
	resp, err := d.call(ctx, creds, map[string]string{
		"Action":           "GetCompShareImageCreateProgress",
		"Region":           firstNonEmpty(region, defaultRegion),
		"Zone":             firstNonEmpty(zone, defaultZone),
		"CompShareImageId": imageID,
	})
	if err != nil {
		return 0, fmt.Errorf("compshare: get custom image progress: %w", err)
	}
	return resp.Process, nil
}

// ListCustomImages lists custom images, optionally filtering by an exact ID.
// Status=Available is the authoritative readiness condition; progress=100 can
// be observed briefly while CompShare is still finalizing the image.
func (d *Driver) ListCustomImages(ctx context.Context, creds provider.Credentials, imageID, region string) ([]CustomImage, error) {
	params := map[string]string{
		"Action": "DescribeCompShareCustomImages",
		"Region": firstNonEmpty(region, defaultRegion),
		"Offset": "0",
		"Limit":  "100",
	}
	if imageID = strings.TrimSpace(imageID); imageID != "" {
		params["CompShareImageId"] = imageID
	}
	resp, err := d.call(ctx, creds, params)
	if err != nil {
		return nil, fmt.Errorf("compshare: list custom images: %w", err)
	}
	images := make([]CustomImage, 0, len(resp.ImageSet))
	for _, image := range resp.ImageSet {
		images = append(images, CustomImage{
			ID: image.CompShareImageID, Name: image.Name, Status: image.Status, CreateTime: image.CreateTime,
		})
	}
	return images, nil
}
