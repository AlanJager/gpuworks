package engine

import (
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

const (
	pinnedDigest = "sha256:22789f0c1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5"
	otherDigest  = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	canonicalPin = "ghcr.io/org/repo@" + pinnedDigest
	mirrorPin    = "ghcr.nju.edu.cn/org/repo@" + pinnedDigest
)

// startImageSpec runs one start attempt and returns every status it reported.
func startImageSpec(t *testing.T, driver runtime.Driver, spec proto.InstanceDesiredState) []proto.InstanceStatus {
	t.Helper()
	h := newSleepHTTPServer(t)
	reported := make(chan proto.InstanceStatus, 64)
	e := NewWithProber(driver, func(st proto.InstanceStatus) { reported <- st }, slog.Default(), t.TempDir(), injectionProber{})
	spec.UUID = "image-prepare"
	spec.APIPort, spec.Ports, spec.StartTimeout = h.port(t), []int{h.port(t)}, 1
	e.startOrAdopt(t.Context(), spec)
	var all []proto.InstanceStatus
	for {
		select {
		case st := <-reported:
			all = append(all, st)
		default:
			return all
		}
	}
}

func failedMessage(statuses []proto.InstanceStatus) string {
	for _, st := range statuses {
		if st.State == proto.InstFailed {
			return st.Message
		}
	}
	return ""
}

// A digest-pinned image fetched through a mirror exists only under the mirror's
// name, and docker cannot tag a digest reference, so the mirror reference has
// to be the one the container is launched from.
func TestDigestPinnedImageIsSatisfiedByAMirrorPinningTheSameDigest(t *testing.T) {
	driver := &imageToleranceDriver{present: map[string]bool{}}
	driver.pullErrs = map[string]error{canonicalPin: errTest("ghcr.io unreachable")}
	statuses := startImageSpec(t, driver, proto.InstanceDesiredState{
		Image: canonicalPin, ImagePullRefs: []string{canonicalPin, mirrorPin},
	})
	if msg := failedMessage(statuses); msg != "" {
		t.Fatalf("start failed: %s", msg)
	}
	if len(driver.tags) != 0 {
		t.Fatalf("a digest-pinned image must not be tagged: %v", driver.tags)
	}
	if len(driver.runImages) != 1 || driver.runImages[0] != mirrorPin {
		t.Fatalf("launched from %v, want the mirror reference %s", driver.runImages, mirrorPin)
	}
	if len(driver.pullRefs) != 2 || driver.pullRefs[0] != canonicalPin || driver.pullRefs[1] != mirrorPin {
		t.Fatalf("pull refs = %v", driver.pullRefs)
	}
}

// A redeploy finds the mirror copy without pulling anything.
func TestDigestPinnedImageAlreadyPresentUnderTheMirrorNameIsNotPulled(t *testing.T) {
	driver := &imageToleranceDriver{present: map[string]bool{mirrorPin: true}}
	statuses := startImageSpec(t, driver, proto.InstanceDesiredState{
		Image: canonicalPin, ImagePullRefs: []string{canonicalPin, mirrorPin},
	})
	if msg := failedMessage(statuses); msg != "" {
		t.Fatalf("start failed: %s", msg)
	}
	if driver.pullCalls != 0 || len(driver.tags) != 0 {
		t.Fatalf("pulls=%d tags=%v, want neither", driver.pullCalls, driver.tags)
	}
	if len(driver.runImages) != 1 || driver.runImages[0] != mirrorPin {
		t.Fatalf("launched from %v, want %s", driver.runImages, mirrorPin)
	}
}

// The canonical digest name itself keeps winning when it is present.
func TestDigestPinnedImagePresentUnderItsOwnNameKeepsTheCanonicalRef(t *testing.T) {
	driver := &imageToleranceDriver{present: map[string]bool{canonicalPin: true, mirrorPin: true}}
	startImageSpec(t, driver, proto.InstanceDesiredState{Image: canonicalPin, ImagePullRefs: []string{mirrorPin, canonicalPin}})
	if driver.pullCalls != 0 || len(driver.runImages) != 1 || driver.runImages[0] != canonicalPin {
		t.Fatalf("pulls=%d launched=%v, want canonical", driver.pullCalls, driver.runImages)
	}
}

// The candidates the server supplies are authoritative: a template declares its
// own image sources and the user picks one, and nothing requires them to pin
// the image's digest. A candidate that pins another digest, or none, is pulled
// like any other and becomes the launch reference; no tag is attempted, which
// docker would refuse for a digest target anyway.
func TestDigestPinnedImageAcceptsAnyServerSuppliedCandidate(t *testing.T) {
	otherPin := "ghcr.nju.edu.cn/org/repo@" + otherDigest
	tagOnly := "ghcr.nju.edu.cn/org/repo:v1"
	for name, tc := range map[string]struct {
		refs     []string
		explicit bool
		pulls    []string
		launch   string
	}{
		"other digest after the unreachable canonical":  {refs: []string{canonicalPin, otherPin, tagOnly}, pulls: []string{canonicalPin, otherPin}, launch: otherPin},
		"tag reference after the unreachable canonical": {refs: []string{canonicalPin, tagOnly}, pulls: []string{canonicalPin, tagOnly}, launch: tagOnly},
		"explicit choice without the canonical name":    {refs: []string{tagOnly}, explicit: true, pulls: []string{tagOnly}, launch: tagOnly},
	} {
		t.Run(name, func(t *testing.T) {
			driver := &imageToleranceDriver{present: map[string]bool{}}
			driver.pullErrs = map[string]error{canonicalPin: errTest("ghcr.io unreachable")}
			statuses := startImageSpec(t, driver, proto.InstanceDesiredState{
				Image: canonicalPin, ImagePullRefs: tc.refs, ImagePullExplicit: tc.explicit,
			})
			if msg := failedMessage(statuses); msg != "" {
				t.Fatalf("start failed: %s", msg)
			}
			if !reflect.DeepEqual(driver.pullRefs, tc.pulls) {
				t.Fatalf("pulled %v, want %v", driver.pullRefs, tc.pulls)
			}
			if len(driver.tags) != 0 {
				t.Fatalf("a digest-pinned image must not be tagged: %v", driver.tags)
			}
			if len(driver.runImages) != 1 || driver.runImages[0] != tc.launch {
				t.Fatalf("launched from %v, want %s", driver.runImages, tc.launch)
			}
		})
	}
}

// A copy of the image under any candidate's name is the image: a redeploy
// finds it without pulling, whatever that candidate pins.
func TestDigestPinnedImageAlreadyPresentUnderAnyCandidateIsNotPulled(t *testing.T) {
	tagOnly := "swr.example.com/org/repo:v1"
	driver := &imageToleranceDriver{present: map[string]bool{tagOnly: true}}
	startImageSpec(t, driver, proto.InstanceDesiredState{Image: canonicalPin, ImagePullRefs: []string{canonicalPin, tagOnly}})
	if driver.pullCalls != 0 || len(driver.tags) != 0 {
		t.Fatalf("pulls=%d tags=%v, want neither", driver.pullCalls, driver.tags)
	}
	if len(driver.runImages) != 1 || driver.runImages[0] != tagOnly {
		t.Fatalf("launched from %v, want %s", driver.runImages, tagOnly)
	}
}

// Every candidate that fails is named in the failure, and nothing launches.
func TestDigestPinnedImageFailsWhenEveryCandidateFails(t *testing.T) {
	otherPin := "ghcr.nju.edu.cn/org/repo@" + otherDigest
	driver := &imageToleranceDriver{present: map[string]bool{}, pullErr: errTest("registry down")}
	statuses := startImageSpec(t, driver, proto.InstanceDesiredState{
		Image: canonicalPin, ImagePullRefs: []string{canonicalPin, otherPin},
	})
	msg := failedMessage(statuses)
	if !strings.Contains(msg, "all image sources failed") {
		t.Fatalf("failure = %q, want all sources failed", msg)
	}
	for _, ref := range []string{canonicalPin, otherPin} {
		if !strings.Contains(msg, ref+": registry down") {
			t.Fatalf("failure %q does not name %s", msg, ref)
		}
	}
	if driver.runCalls != 0 || len(driver.tags) != 0 {
		t.Fatalf("run=%d tags=%v after every source failed", driver.runCalls, driver.tags)
	}
}

// The progress stream names the source the container is launched from when it
// is not the instance's own image, and stays quiet when it is.
func TestImagePreparationNamesTheLaunchReferenceOnlyWhenItDiffers(t *testing.T) {
	messages := func(statuses []proto.InstanceStatus) string {
		var all []string
		for _, st := range statuses {
			all = append(all, st.Message)
		}
		return strings.Join(all, "\n")
	}
	pulled := startImageSpec(t, &imageToleranceDriver{present: map[string]bool{}}, proto.InstanceDesiredState{
		Image: canonicalPin, ImagePullRefs: []string{mirrorPin}, ImagePullExplicit: true,
	})
	if !strings.Contains(messages(pulled), "launching from "+mirrorPin) {
		t.Fatalf("pulled stream does not name the launch reference:\n%s", messages(pulled))
	}
	present := startImageSpec(t, &imageToleranceDriver{present: map[string]bool{mirrorPin: true}}, proto.InstanceDesiredState{
		Image: canonicalPin, ImagePullRefs: []string{canonicalPin, mirrorPin},
	})
	if !strings.Contains(messages(present), "image is already available locally; launching from "+mirrorPin) {
		t.Fatalf("present stream does not name the launch reference:\n%s", messages(present))
	}
	canonical := startImageSpec(t, &imageToleranceDriver{present: map[string]bool{}}, proto.InstanceDesiredState{Image: canonicalPin})
	if strings.Contains(messages(canonical), "launching from") {
		t.Fatalf("canonical pull must not announce a different launch reference:\n%s", messages(canonical))
	}
}

// A tag-referenced image keeps the tag path: the mirror pull is tagged back to
// the canonical name and the container is launched from the canonical name.
func TestTaggedImageStillTagsAMirrorPullToTheCanonicalRef(t *testing.T) {
	driver := &imageToleranceDriver{present: map[string]bool{}}
	statuses := startImageSpec(t, driver, proto.InstanceDesiredState{
		Image: "ghcr.io/org/repo:v1", ImagePullRefs: []string{"ghcr.nju.edu.cn/org/repo:v1"},
	})
	if msg := failedMessage(statuses); msg != "" {
		t.Fatalf("start failed: %s", msg)
	}
	if len(driver.tags) != 1 || driver.tags[0] != [2]string{"ghcr.nju.edu.cn/org/repo:v1", "ghcr.io/org/repo:v1"} {
		t.Fatalf("tags = %v", driver.tags)
	}
	if len(driver.runImages) != 1 || driver.runImages[0] != "ghcr.io/org/repo:v1" {
		t.Fatalf("launched from %v, want the canonical tag", driver.runImages)
	}
}

// A tag-referenced image is not satisfied by a mirror copy that is only
// present under the mirror's name: that is the tag path's job.
func TestTaggedImageIgnoresAMirrorNamePresentLocally(t *testing.T) {
	driver := &imageToleranceDriver{present: map[string]bool{"ghcr.nju.edu.cn/org/repo:v1": true}}
	startImageSpec(t, driver, proto.InstanceDesiredState{
		Image: "ghcr.io/org/repo:v1", ImagePullRefs: []string{"ghcr.nju.edu.cn/org/repo:v1"},
	})
	if driver.pullCalls != 1 || len(driver.runImages) != 1 || driver.runImages[0] != "ghcr.io/org/repo:v1" {
		t.Fatalf("pulls=%d launched=%v", driver.pullCalls, driver.runImages)
	}
}

// When the pull errors but the image shows up (another pull won the race), the
// recheck accepts the mirror reference for a pinned image too, and the failure
// stays reported when nothing is there.
func TestDigestPinnedImageRecheckAfterAFailedPull(t *testing.T) {
	racing := &imageToleranceDriver{present: map[string]bool{}, pullErrs: map[string]error{mirrorPin: errTest("connection reset")}}
	racing.hook = func(ref string) { racing.present[mirrorPin] = true }
	startImageSpec(t, racing, proto.InstanceDesiredState{Image: canonicalPin, ImagePullRefs: []string{mirrorPin}})
	if len(racing.runImages) != 1 || racing.runImages[0] != mirrorPin {
		t.Fatalf("recheck: launched from %v, want %s", racing.runImages, mirrorPin)
	}

	failing := &imageToleranceDriver{present: map[string]bool{}, pullErrs: map[string]error{mirrorPin: errTest("connection reset")}}
	statuses := startImageSpec(t, failing, proto.InstanceDesiredState{Image: canonicalPin, ImagePullRefs: []string{mirrorPin}})
	msg := failedMessage(statuses)
	if !strings.Contains(msg, mirrorPin+": connection reset") || failing.runCalls != 0 {
		t.Fatalf("failure = %q, run calls = %d", msg, failing.runCalls)
	}
}

func TestImageDigest(t *testing.T) {
	for ref, want := range map[string]string{
		canonicalPin: pinnedDigest,
		"repo:v1@" + strings.ToUpper(pinnedDigest[:7]) + pinnedDigest[7:]: pinnedDigest,
		"repo:v1":      "",
		"repo@":        "",
		"repo@sha256:": "",
		"repo@md5:abc": "",
	} {
		if got := imageDigest(ref); got != want {
			t.Errorf("imageDigest(%q) = %q, want %q", ref, got, want)
		}
	}
}
