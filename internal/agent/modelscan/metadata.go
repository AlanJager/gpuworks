package modelscan

import (
	"context"
	"path/filepath"
	"slices"
	"strings"

	"gpuworks.ai/gpuworks/pkg/model/modelmetadata"
)

// ParseMetadata parses dir using that directory as its file scope.
func ParseMetadata(ctx context.Context, dir string) (modelmetadata.Report, error) {
	v, err := OpenView(dir)
	if err != nil {
		return modelmetadata.Report{}, err
	}
	defer v.Close()
	return modelmetadata.Parse(ctx, modelmetadata.Input{Path: "."}, v)
}

// ParseMetadataWithin rebuilds ancestor metadata within root, then parses dir.
func ParseMetadataWithin(ctx context.Context, root, dir string) (modelmetadata.Report, error) {
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return modelmetadata.Report{}, err
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return modelmetadata.Report{}, err
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return modelmetadata.Report{}, modelmetadata.ErrOutsideScope
	}
	if rel == "." {
		return ParseMetadata(ctx, dir)
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) > 64 {
		return modelmetadata.Report{}, modelmetadata.ErrReadLimit
	}
	s := scanner{root: root}
	n := node{}
	var report modelmetadata.Report
	for i, part := range parts {
		if n.rel == "" {
			n.rel = part
		} else {
			n.rel += "/" + part
		}
		report, err = s.parseNode(ctx, n)
		if err != nil {
			return report, err
		}
		if report.Prune {
			return report, nil
		}
		if i == len(parts)-1 {
			break
		}
		n.inheritMetadata(report)
	}
	return report, nil
}

// inheritMetadata advances repository ownership for descendants. Clone slices
// before extending them so queued siblings never share mutable context.
func (n *node) inheritMetadata(report modelmetadata.Report) {
	if n.scope == "" && (report.Repository || len(report.Components) > 0) {
		n.scope = n.rel
	}
	if n.scope != "" {
		n.context.Repository = n.context.Repository || report.Repository
		n.context.Components = append(slices.Clone(n.context.Components), report.Components...)
		n.context.Tasks = slices.Clone(report.Tasks)
		n.context.TaskPriority = report.TaskPriority
	}
}
