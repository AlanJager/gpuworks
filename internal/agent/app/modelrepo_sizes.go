package app

import (
	"net/http"
	"os"
	"strings"
	"sync"

	"gpuworks.ai/gpuworks/internal/agent/modelfetch"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

// approximateSizeHeadroom is the extra share of a download budgeted when the
// repository listing only carries display sizes. A file server prints two
// significant digits ("1.4G"), so a listed size can under-report the file by
// up to about 5%; one sixteenth (6.25%) covers that with a margin, on top of
// the fixed safety margin every download already reserves.
const approximateSizeHeadroom = 16

// admissionBytes is how much of a download's remaining size must be found free
// before it starts. A listing that only carries display sizes can under-report
// every file, so the budget carries the extra share as well; the fixed safety
// margin checkModelDownloadSpace adds is on top of this.
func admissionBytes(remaining int64, approximate bool) int64 {
	if remaining <= 0 || !approximate {
		return remaining
	}
	budget := remaining + remaining/approximateSizeHeadroom
	if budget < remaining {
		return remaining
	}
	return budget
}

// repoSizeEstimate keeps a repository's byte total truthful while it is being
// downloaded.
//
// A directory index prints sizes for people, so the total a download starts
// from is an estimate. Each file's real length — the Content-Length the
// transfer reports, or the size the finished file has on disk — replaces that
// file's estimate as soon as it is known, so the reported total converges on
// the exact size instead of leaving progress stuck at an impossible
// percentage. Every counted file contributes at least as many bytes as it has
// transferred, which is what keeps completed bytes from ever exceeding the
// total. For a source whose listing is already exact, every correction is the
// identity.
type repoSizeEstimate struct {
	mu    sync.Mutex
	files []modelrepo.File
	bytes int64
}

func newRepoSizeEstimate(files []modelrepo.File, total int64) *repoSizeEstimate {
	manifest := make([]modelrepo.File, len(files))
	copy(manifest, files)
	return &repoSizeEstimate{files: manifest, bytes: total}
}

// correct replaces one file's counted size with its measured size.
func (e *repoSizeEstimate) correct(index int, measured int64) {
	if index < 0 || measured < 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if index >= len(e.files) || e.files[index].Size == measured {
		return
	}
	e.bytes += measured - e.files[index].Size
	e.files[index].Size = measured
	if e.bytes < 0 {
		e.bytes = 0
	}
}

// Total is the repository size as currently known.
func (e *repoSizeEstimate) Total() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.bytes
}

// Manifest is the file list with every size correction applied.
func (e *repoSizeEstimate) Manifest() []modelrepo.File {
	e.mu.Lock()
	defer e.mu.Unlock()
	manifest := make([]modelrepo.File, len(e.files))
	copy(manifest, e.files)
	return manifest
}

// repoFileMeasured reports whether a repository file is already complete on
// disk, and how large it actually is.
//
// A listed size that is exact is compared exactly. A display size cannot be:
// it never equals the file's real length, so completeness is decided by the
// only evidence the downloader leaves — a finished file is renamed into place,
// while an interrupted one stays in its ".part" sibling. A present, non-empty
// regular file is therefore a complete file.
func repoFileMeasured(dest string, listed int64, approximate bool) (int64, bool) {
	info, err := os.Stat(dest)
	if err != nil || !info.Mode().IsRegular() {
		return 0, false
	}
	if approximate {
		if info.Size() <= 0 {
			return 0, false
		}
		return info.Size(), true
	}
	if info.Size() != listed {
		return 0, false
	}
	return info.Size(), true
}

// modelSourceClient is the HTTP client a model source's requests go through on
// this node: the node's proxy, which skips the hosts its no_proxy lists. An
// HTTP directory is additionally confined to the host it was registered with
// by the shared directory client.
func modelSourceClient(source, endpoint string, net proto.NetworkConfig) (*http.Client, error) {
	base := httpClientFor(net)
	if source != modelrepo.SourceURL {
		return base, nil
	}
	return modelrepo.DirectoryClient(endpoint, base)
}

// fetchCandidates are the addresses a download task is fetched from. A
// server that plans them sends them (proto.ModelFileTask.Candidates); one
// that predates them sends one endpoint at most, which the node's URL
// rewrites may still redirect - the only way an older server named a
// mirror. Remove the fallback once servers older than candidates are no
// longer supported.
func fetchCandidates(mf proto.ModelFileTask, net proto.NetworkConfig) []proto.ModelFetchCandidate {
	if len(mf.Candidates) > 0 {
		return mf.Candidates
	}
	base := strings.TrimRight(strings.TrimSpace(mf.Endpoint), "/")
	if base == "" {
		base = modelrepo.BaseURL(mf.Source, "")
	}
	if base == "" {
		return nil
	}
	if rewrite := sourceURLRewrite(mf.Source, net); rewrite != nil {
		// A rewrite names a prefix of request URLs, which may end in "/".
		base = strings.TrimRight(rewrite(base+"/"), "/")
	}
	return []proto.ModelFetchCandidate{{Base: base, Upstream: base == modelrepo.BaseURL(mf.Source, mf.Endpoint)}}
}

// downloadingFrom is the progress message naming where the bytes come from;
// the console reads the host from it (ui modelPresenceDisplay.transferOrigin).
func downloadingFrom(host string) string {
	return "downloading from " + host
}

// switchedMessage is downloadingFrom after the download left an address,
// with the reason it did.
func switchedMessage(s modelfetch.Switch) string {
	return downloadingFrom(s.To) + " (switched: " + s.Reason + ")"
}

// sourceURLRewrite reports the URL rewrite a source may use. A directory URL
// is already the address of the bytes, so a mirror rule — which only swaps one
// host prefix for another — cannot produce a working address for it and must
// not be allowed to send the download somewhere else.
func sourceURLRewrite(source string, net proto.NetworkConfig) func(string) string {
	if source == modelrepo.SourceURL {
		return nil
	}
	return net.Apply
}
