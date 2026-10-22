package runtime

import "testing"

// The exact text is a contract with the node's pull parser and with what the
// operator reads; these are the lines the drivers printed before the
// constructors existed, byte for byte.
func TestPullLinesAreExactly(t *testing.T) {
	const hex = "a171f301df7b8d27e72eb53099ab1a7b548ec5b3ac8baf546ea16d2fa64c46aa"
	for got, want := range map[string]string{
		PullLineDownloading("sha256:"+hex, 10, 100):           "sha256:" + hex + ": Downloading 10 B/100 B",
		PullLineDownloading(hex, 10, 100):                     "sha256:" + hex + ": Downloading 10 B/100 B",
		PullLineExtracting("sha256:"+hex, 0, 100):             "sha256:" + hex + ": Extracting 0 B/100 B",
		PullLineDownloadComplete(hex):                         "sha256:" + hex + ": Download complete",
		PullLineAlreadyExists("sha256:" + hex):                "sha256:" + hex + ": Already exists",
		PullLineDownloadingInSteps(hex, 1, 2, 107374182):      "sha256:" + hex + ": Downloading 1 B/2 B (in steps of 107374182 B)",
		PullLineDownloadingInSteps(hex, 1, 2, 0):              "sha256:" + hex + ": Downloading 1 B/2 B",
		PullLineExtractingLayers(0):                           "Extracting downloaded layers",
		PullLineExtractingLayers(11):                          "Extracting downloaded layers (11s)",
		PullLineUnreadable(`nerdctl blob status "unpacking"`): "Progress output not understood, stall check off: nerdctl blob status \"unpacking\"",
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestIsPullLineUnreadableOnlyForItsOwnLines(t *testing.T) {
	if !IsPullLineUnreadable(PullLineUnreadable("x")) {
		t.Fatal("its own line was not recognised")
	}
	for _, line := range []string{"", "Extracting downloaded layers", PullLineDownloading("abc", 1, 2), "progress output not understood"} {
		if IsPullLineUnreadable(line) {
			t.Fatalf("%q taken for an unreadable notice", line)
		}
	}
}
