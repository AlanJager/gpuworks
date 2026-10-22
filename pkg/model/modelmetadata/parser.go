package modelmetadata

import (
	"context"
	"errors"
	"io/fs"
	"path"
	"slices"
	"strings"
)

type Parser struct{ rules []Rule }

func NewParser(rules ...Rule) *Parser { return &Parser{rules: slices.Clone(rules)} }

var defaultParser = NewParser(
	ruleFunc(parseRepository), ruleFunc(parseTransformers),
	ruleFunc(parseSentenceTransformers), ruleFunc(parseDiffusers),
	ruleFunc(parseModelScope), ruleFunc(parseMistral),
	ruleFunc(parseGGUF), ruleFunc(parseAdapter), ruleFunc(parseExports),
)

func Parse(ctx context.Context, input Input, files FileView) (Report, error) {
	return defaultParser.Parse(ctx, input, files)
}

func (p *Parser) Parse(ctx context.Context, input Input, source FileView) (Report, error) {
	if input.Path == "" {
		input.Path = "."
	}
	if !fs.ValidPath(input.Path) {
		return Report{}, ErrOutsideScope
	}
	files := cacheView(source)
	if _, err := files.ReadDir(ctx, input.Path); err != nil {
		return Report{}, err
	}
	out := Report{}
	for _, rule := range p.rules {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		result, err := rule.Parse(ctx, input, files)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return out, err
			}
			out.Issues = append(out.Issues, issue(input.Path, "metadata_unreadable", err))
			continue
		}
		out.Entries = append(out.Entries, result.Entries...)
		out.Components = append(out.Components, result.Components...)
		out.Evidence = append(out.Evidence, result.Evidence...)
		out.Issues = append(out.Issues, result.Issues...)
		out.Repository = out.Repository || result.Repository
		out.Prune = out.Prune || result.Prune
	}
	docs, issues := readMetadata(ctx, input.Path, files)
	out.ConfigRaw, out.README = docs.Config, docs.README
	out.Issues = append(out.Issues, issues...)
	tasks := InferTasks(docs)
	out.Tasks = tasks.Tasks
	out.TaskPriority = tasks.Priority
	out.Evidence = append(out.Evidence, tasks.Evidence...)
	out.Issues = append(out.Issues, tasks.Issues...)
	if !tasks.Declared && input.Context.Repository && len(input.Context.Tasks) > 0 &&
		(len(out.Tasks) == 0 || (tasks.Priority <= architectureTaskPriority && input.Context.TaskPriority > tasks.Priority)) {
		out.Tasks = slices.Clone(input.Context.Tasks)
		out.TaskPriority = input.Context.TaskPriority
		out.Evidence = append(out.Evidence, Evidence{Source: "repository context", Field: "tasks", Value: strings.Join(out.Tasks, ",")})
	}
	for _, ref := range input.Context.Components {
		if ref.Path != "" && (input.Path == ref.Path || strings.HasPrefix(input.Path, ref.Path+"/")) {
			out.Component = true
		}
	}
	for _, ref := range out.Components {
		if ref.Path == input.Path && (ref.Role == "adapter" || ref.Role == "component") {
			out.Component = true
		}
	}
	out.Entries = mergeEntries(out.Entries)
	if out.Component {
		out.Entries = slices.DeleteFunc(out.Entries, func(e Entry) bool { return !e.Independent })
		if len(out.Entries) == 0 {
			out.Tasks = nil
			out.TaskPriority = 0
		}
	}
	for i := range out.Entries {
		out.Entries[i].Tasks = slices.Clone(out.Tasks)
	}
	if len(out.Entries) > 0 && len(out.Tasks) == 0 {
		out.Issues = append(out.Issues, issue(input.Path, "unknown_task", nil))
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	return out, nil
}

func mergeEntries(entries []Entry) []Entry {
	// Specific composite layouts describe the same entry more precisely than
	// the Transformers checkpoint that is one of their components.
	return slices.DeleteFunc(slices.Clone(entries), func(e Entry) bool {
		for _, other := range entries {
			if other.Path == e.Path && slices.Contains(other.Supersedes, e.Format) {
				return true
			}
		}
		return false
	})
}

func parseRepository(ctx context.Context, in Input, files FileView) (RuleResult, error) {
	items, err := files.ReadDir(ctx, in.Path)
	if err != nil {
		return RuleResult{}, err
	}
	r := RuleResult{}
	if hasName(items, "blobs") && hasName(items, "snapshots") {
		r.Prune = true
		r.Issues = append(r.Issues, issue(in.Path, "hf_hub_cache_layout", nil))
	}
	for _, name := range []string{".gpuworks-model-complete.json", ".msc", ".mv"} {
		if hasName(items, name) {
			r.Repository = true
		}
	}
	for _, name := range []string{".huggingface/download", ".cache/huggingface/download"} {
		if info, err := files.Stat(ctx, path.Join(in.Path, name)); err == nil && info.IsDir {
			r.Repository = true
		}
	}
	card, _ := readModelCard(ctx, in.Path, files)
	if card.Library != "" || len(card.PipelineTags) > 0 {
		r.Repository = true
	}
	return r, nil
}
