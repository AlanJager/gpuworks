package modelmetadata

import (
	"context"
	"errors"
	"path"
	"strings"
)

var errorsTruncatedCard = errors.New("unterminated model-card metadata")

func readModelCard(ctx context.Context, dir string, files FileView) (modelCard, error) {
	body, err := readREADME(ctx, dir, files)
	if err != nil {
		return modelCard{}, err
	}
	return parseModelCard(string(body))
}

func readREADME(ctx context.Context, dir string, files FileView) ([]byte, error) {
	items, err := files.ReadDir(ctx, dir)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if !item.IsDir && strings.EqualFold(item.Name, "README.md") {
			return readDocument(ctx, files, path.Join(dir, item.Name))
		}
	}
	return nil, nil
}

func readMetadata(ctx context.Context, dir string, files FileView) (Documents, []Issue) {
	d := Documents{}
	var issues []Issue
	for _, item := range []struct {
		name   string
		target *string
	}{
		{"config.json", &d.Config}, {"configuration.json", &d.Configuration}, {"modules.json", &d.Modules},
	} {
		body, err := readDocument(ctx, files, path.Join(dir, item.name))
		if err != nil {
			issues = append(issues, issue(path.Join(dir, item.name), "metadata_unreadable", err))
			continue
		}
		*item.target = string(body)
	}
	body, err := readREADME(ctx, dir, files)
	if err != nil {
		issues = append(issues, issue(path.Join(dir, "README.md"), "metadata_unreadable", err))
	} else {
		d.README = string(body)
	}
	return d, issues
}
