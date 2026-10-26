package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	serverprovider "gpuworks.ai/gpuworks/internal/server/provider"
	aliyundriver "gpuworks.ai/gpuworks/internal/server/provider/aliyun"
)

// imageAPI is the custom-image slice of the ECS API. Images are region-scoped:
// CopyImage makes one copy per destination region, each with its own ID.
type imageAPI struct {
	rpc   *aliyundriver.RPC
	creds serverprovider.Credentials
}

func (a imageAPI) call(ctx context.Context, region, action string, params map[string]string, out any) error {
	return a.rpc.Call(ctx, a.creds, "ecs", region, action, params, out)
}

// create snapshots the instance's system disk into a custom image. The tag
// attributes the image (and its snapshot) to a golden-image build.
func (a imageAPI) create(ctx context.Context, region, instanceID, name, description string) (string, error) {
	var out struct {
		ImageID string `json:"ImageId"`
	}
	err := a.call(ctx, region, "CreateImage", map[string]string{
		"InstanceId": instanceID, "ImageName": name, "Description": description,
		"Tag.1.Key": "gpuworks-golden", "Tag.1.Value": "1",
	}, &out)
	if err != nil {
		return "", fmt.Errorf("create image %q from %s: %w", name, instanceID, err)
	}
	if out.ImageID == "" {
		return "", fmt.Errorf("create image %q returned no image ID", name)
	}
	return out.ImageID, nil
}

// copy starts the copy of an Available image to another region and returns the
// ID of the copy there.
func (a imageAPI) copy(ctx context.Context, srcRegion, imageID, dstRegion, name, description string) (string, error) {
	var out struct {
		ImageID string `json:"ImageId"`
	}
	err := a.call(ctx, srcRegion, "CopyImage", map[string]string{
		"ImageId": imageID, "DestinationRegionId": dstRegion,
		"DestinationImageName": name, "DestinationDescription": description,
		"Tag.1.Key": "gpuworks-golden", "Tag.1.Value": "1",
	}, &out)
	if err != nil {
		return "", fmt.Errorf("copy image %s from %s to %s: %w", imageID, srcRegion, dstRegion, err)
	}
	if out.ImageID == "" {
		return "", fmt.Errorf("copy image %s to %s returned no image ID", imageID, dstRegion)
	}
	return out.ImageID, nil
}

type imageState struct {
	ImageID  string `json:"ImageId"`
	Name     string `json:"ImageName"`
	Status   string `json:"Status"`
	Progress string `json:"Progress"`
	Size     int    `json:"Size"`
}

// describe returns the images matching the filter in any state: DescribeImages
// lists only Available images unless asked for the others.
func (a imageAPI) describe(ctx context.Context, region string, filter map[string]string) ([]imageState, error) {
	params := map[string]string{"Status": "Creating,Waiting,Available,UnAvailable,CreateFailed,Deprecated", "ImageOwnerAlias": "self", "PageSize": "50"}
	for k, v := range filter {
		params[k] = v
	}
	var out struct {
		Images struct {
			Image []imageState `json:"Image"`
		} `json:"Images"`
	}
	if err := a.call(ctx, region, "DescribeImages", params, &out); err != nil {
		return nil, err
	}
	return out.Images.Image, nil
}

// wait polls until the image is Available. A terminal failure state ends the
// wait at once instead of burning the timeout.
func (a imageAPI) wait(ctx context.Context, region, imageID string, every time.Duration, progress func(string)) error {
	for {
		images, err := a.describe(ctx, region, map[string]string{"ImageId": imageID})
		if err == nil {
			if len(images) == 1 {
				switch images[0].Status {
				case "Available":
					return nil
				case "CreateFailed", "UnAvailable":
					return fmt.Errorf("image %s in %s is %s", imageID, region, images[0].Status)
				}
				if progress != nil {
					progress(fmt.Sprintf("region=%s image=%s status=%s progress=%s", region, imageID, images[0].Status, images[0].Progress))
				}
			}
		} else if progress != nil {
			progress(fmt.Sprintf("region=%s image=%s describe: %v", region, imageID, err))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for image %s in %s: %w", imageID, region, ctx.Err())
		case <-time.After(every):
		}
	}
}

// byName finds the newest image with exactly this name (resuming a build).
func (a imageAPI) byName(ctx context.Context, region, name string) (imageState, error) {
	images, err := a.describe(ctx, region, map[string]string{"ImageName": name})
	if err != nil {
		return imageState{}, err
	}
	for _, image := range images {
		if image.Name == name {
			return image, nil
		}
	}
	return imageState{}, fmt.Errorf("no image named %q in %s", name, region)
}

// envVarName is the environment variable the L4 harness reads a region's image
// from: ALIYUN_IMAGE_CN_HANGZHOU for cn-hangzhou.
func envVarName(region string) string {
	return "ALIYUN_IMAGE_" + strings.ToUpper(strings.ReplaceAll(region, "-", "_"))
}
