// Package modelmetadata interprets model files without depending on a storage,
// transport, database or inference engine. Format rules read through FileView.
package modelmetadata

import (
	"context"
	"errors"
)

var (
	ErrOutsideScope = errors.New("path is outside the metadata context")
	ErrReadLimit    = errors.New("metadata read limit exceeded")
)

type FileInfo struct {
	Name  string
	IsDir bool
	Size  int64
}

// Paths are relative to the view, including references to sibling components.
// ReadRange returns io.EOF for a short read; implementations must not silently
// truncate a requested range or follow a symlink outside the view.
type FileView interface {
	ReadDir(context.Context, string) ([]FileInfo, error)
	Stat(context.Context, string) (FileInfo, error)
	ReadRange(context.Context, string, int64, int64) ([]byte, error)
}

type Input struct {
	Path    string
	Context Context
}

// Context contains only evidence from an already identified repository. All
// paths use the same FileView coordinates as Input.Path.
type Context struct {
	Repository   bool
	Components   []Reference
	Tasks        []string
	TaskPriority int
}

type Entry struct {
	Path       string   `json:"path"`
	Format     string   `json:"format"`
	Files      []string `json:"files,omitempty"`
	Tasks      []string `json:"tasks,omitempty"`
	Incomplete bool     `json:"incomplete,omitempty"`
	Missing    []string `json:"missing,omitempty"`
	// A manifest can establish an entry even inside another pipeline.
	Independent bool `json:"-"`
	// A specific rule may replace a generic interpretation of the same path.
	Supersedes []string `json:"-"`
}

type Reference struct {
	Path       string `json:"path,omitempty"`
	Role       string `json:"role"`
	Repository string `json:"repository,omitempty"`
	Subfolder  string `json:"subfolder,omitempty"`
	Source     string `json:"source"`
}

type Evidence struct {
	Source string `json:"source"`
	Field  string `json:"field"`
	Value  string `json:"value"`
}

type Issue struct {
	Path   string `json:"path"`
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

type Report struct {
	Entries      []Entry     `json:"entries,omitempty"`
	Components   []Reference `json:"components,omitempty"`
	Tasks        []string    `json:"tasks,omitempty"`
	TaskPriority int         `json:"-"`
	Evidence     []Evidence  `json:"evidence,omitempty"`
	Issues       []Issue     `json:"issues,omitempty"`
	Repository   bool        `json:"repository,omitempty"`
	Component    bool        `json:"component,omitempty"`
	// Prune applies to infrastructure layouts such as the Hub blob cache,
	// never merely because a model was found in the current directory.
	Prune     bool   `json:"prune,omitempty"`
	ConfigRaw string `json:"config_raw,omitempty"`
	README    string `json:"readme,omitempty"`
}

type RuleResult struct {
	Entries    []Entry
	Components []Reference
	Evidence   []Evidence
	Issues     []Issue
	Repository bool
	Prune      bool
}

type Rule interface {
	Parse(context.Context, Input, FileView) (RuleResult, error)
}

type ruleFunc func(context.Context, Input, FileView) (RuleResult, error)

func (f ruleFunc) Parse(ctx context.Context, in Input, files FileView) (RuleResult, error) {
	return f(ctx, in, files)
}
