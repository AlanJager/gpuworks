package proto

// Final repository-listing outcomes produced by the agent. They describe the
// attempted acquisition, never a change to the model's immutable identity.
const (
	ErrorModelDownloadNetworkExhausted   = "MODEL_DOWNLOAD_NETWORK_EXHAUSTED"
	ErrorModelDownloadSourceNotFound     = "MODEL_DOWNLOAD_SOURCE_NOT_FOUND"
	ErrorModelDownloadSourceAccessDenied = "MODEL_DOWNLOAD_SOURCE_ACCESS_DENIED"
	ErrorModelDownloadSourceThrottled    = "MODEL_DOWNLOAD_SOURCE_THROTTLED"
	ErrorModelDownloadSourceRejected     = "MODEL_DOWNLOAD_SOURCE_REJECTED"
)

func IsModelDownloadErrorCode(code string) bool {
	switch code {
	case ErrorModelDownloadNetworkExhausted, ErrorModelDownloadSourceNotFound,
		ErrorModelDownloadSourceAccessDenied, ErrorModelDownloadSourceThrottled,
		ErrorModelDownloadSourceRejected:
		return true
	default:
		return false
	}
}
