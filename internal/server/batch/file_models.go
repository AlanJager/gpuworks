package batch

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
)

// Limits on how a refusal or warning about the models of a file is worded and
// how far the lookups go.
const (
	// namedModels is how many models a message names.
	namedModels = 5
	// maxReportedModels stops looking models up once this many are unknown:
	// the message names a few and says there are more, and a file of fifty
	// thousand made-up names must not cost fifty thousand lookups.
	maxReportedModels = 20
)

// ModelsText names the first few of models, with an ellipsis when more
// follow. It is language-neutral, for a message of any language to embed.
func ModelsText(models []string) string {
	if len(models) > namedModels {
		return strings.Join(models[:namedModels], ", ") + ", ..."
	}
	return strings.Join(models, ", ")
}

// fileModels reads the distinct body.model values of a stored input file in
// the order they first appear. It streams the file and keeps one set of names,
// so its memory does not grow with the file. The file was validated when it was
// uploaded, so a line that does not parse is skipped, not reported.
func (s *Service) fileModels(f File) ([]string, error) {
	path, err := s.filePath(f.ID)
	if err != nil {
		return nil, notFound("file_not_found", "No such File object: "+f.ID)
	}
	fh, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, notFound("file_not_found", "The file's content is no longer available: "+f.ID)
		}
		return nil, err
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 64<<10), MaxLineBytes+1)
	seen := map[string]struct{}{}
	var names []string
	for sc.Scan() {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var l struct {
			Body struct {
				Model string `json:"model"`
			} `json:"body"`
		}
		if json.Unmarshal(raw, &l) != nil || l.Body.Model == "" {
			continue
		}
		if _, dup := seen[l.Body.Model]; !dup {
			seen[l.Body.Model] = struct{}{}
			names = append(names, l.Body.Model)
		}
	}
	return names, sc.Err()
}

// checkFileModels is the model check of a batch without an override: every
// request goes to the model its line names, so a model no active deployment
// serves would fail all of its requests at once. Such a file is refused;
// models whose deployments sleep by an operator's or a schedule's order are
// only warned about, as an override naming one is.
func (s *Service) checkFileModels(f File) ([]Warning, error) {
	if s.cfg.Models == nil {
		return nil, nil
	}
	names, err := s.fileModels(f)
	if err != nil {
		return nil, err
	}
	var unknown, held []string
	for _, name := range names {
		state, err := s.cfg.Models.Lookup(name)
		if err != nil {
			return nil, err
		}
		switch state {
		case ModelSleepHeld:
			held = append(held, name)
		case ModelAvailable:
		default:
			unknown = append(unknown, name)
		}
		if len(unknown) >= maxReportedModels {
			break
		}
	}
	if len(unknown) > 0 {
		e := invalid("model_not_found", "input_file_id", "The input file names models that no active deployment serves: "+ModelsText(unknown)+
			". Set a model to send every request to, or deploy these models first")
		e.Models = unknown
		return nil, e
	}
	if len(held) > 0 {
		return []Warning{{
			Code: WarningModelSleepHeld, Param: "input_file_id", Models: held,
			Message: "The deployments serving " + ModelsText(held) + " sleep by an operator's or a schedule's order, which requests do not wake; their requests are retried until they are woken or the batch expires",
		}}, nil
	}
	return nil, nil
}
