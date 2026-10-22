package dto

import "time"

// Storage files (ADR 0053): browsing a storage's tree and changing the
// regions of it open to writes. Paths are relative to the storage root,
// slash separated; "" is the root.

// StorageFileList is one page of a directory listing. Writable says whether
// the directory takes new entries here (a writable region, the storage
// enabled, the node able to write); NextCursor is "" on the last page.
type StorageFileList struct {
	Path       string            `json:"path"`
	NodeUUID   string            `json:"node_uuid"`
	Writable   bool              `json:"writable"`
	Items      []StorageFileItem `json:"items"`
	NextCursor string            `json:"next_cursor"`
}

// StorageFileItem is one directory entry, as lstat sees it: a symbolic link
// is reported as one and never followed. Writable says whether it can be
// renamed or deleted.
type StorageFileItem struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Dir      bool      `json:"dir"`
	Symlink  bool      `json:"symlink"`
	Size     int64     `json:"size"`
	ModTime  time.Time `json:"mtime"`
	Writable bool      `json:"writable"`
}

// StorageFileMkdirRequest creates a directory (and missing parents) inside a
// writable region.
type StorageFileMkdirRequest struct {
	NodeUUID string `json:"node_uuid"`
	Path     string `json:"path"`
}

// StorageFileRenameRequest moves an entry within the writable regions.
type StorageFileRenameRequest struct {
	NodeUUID string `json:"node_uuid"`
	From     string `json:"from"`
	To       string `json:"to"`
}

// StorageFilePath answers a mkdir or a rename with the path now in place.
type StorageFilePath struct {
	Path string `json:"path"`
}

// StorageFileUploadResult answers an upload: what the agent received and how
// many regular files the target now holds from it (1 without extraction).
type StorageFileUploadResult struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
	Files int    `json:"files"`
}
