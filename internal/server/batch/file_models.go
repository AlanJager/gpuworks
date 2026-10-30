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

// NamedModels splits models into the first few a message names and the
// number of those that follow, for a client to word in its own language.
func NamedModels(models []string) (named []string, more int) {
	if len(models) > namedModels {
		return models[:namedModels], len(models) - namedModels
	}
	return models, 0
}

// ModelsText names the first few of models in English punctuation, with an
// ellipsis when more follow: the wording of the OpenAI-compatible messages.
func ModelsText(models []string) string {
	named, more := NamedModels(models)
	if more > 0 {
		return strings.Join(named, ", ") + ", ..."
	}
	return strings.Join(named, ", ")
}

// fileModels reads the distinct body.model values of a stored input file in
// the order they first appear. It streams the file and keeps one set of names,
// so its memory does not grow with the file. The file was validated when it was
// uploaded, so a line that does not parse is skipped, not reported.
func (s *Service) fileModels(f File) ([]string, error) {
	path, err := s.filePath(f.ID)
	if err != nil {
		return nil, notFound(CodeFileNotFound, "No such File object: "+f.ID)
	}
	fh, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, notFound(CodeFileNotFound, "The file's content is no longer available: "+f.ID)
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
	unknown, held, err := s.classify(names)
	if err != nil {
		return nil, err
	}
	if len(unknown) > 0 {
		e := invalid(CodeModelNotFound, ParamInputFileID, "The input file names models that no active deployment serves: "+ModelsText(unknown)+
			". Set a model to send every request to, or deploy these models first")
		e.Models = unknown
		return nil, e
	}
	return heldWarnings(ParamInputFileID, held), nil
}

// classify looks names up and returns those no active deployment serves and
// those whose every deployment sleeps by an operator's or a schedule's order,
// each in the order of names. It stops once maxReportedModels are unknown.
func (s *Service) classify(names []string) (unknown, held []string, err error) {
	for _, name := range names {
		state, err := s.cfg.Models.Lookup(name)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case state.Accepts():
		case state == ModelSleepHeld:
			held = append(held, name)
		default:
			unknown = append(unknown, name)
		}
		if len(unknown) >= maxReportedModels {
			break
		}
	}
	return unknown, held, nil
}

// heldWarnings is the warning about models whose deployments sleep by an
// operator's or a schedule's order, none when held is empty; param is the
// request field that chose them.
func heldWarnings(param string, held []string) []Warning {
	if len(held) == 0 {
		return nil
	}
	message := "The deployment serving " + held[0] + " sleeps by an operator's or a schedule's order, which requests do not wake; the batch's requests are retried until it is woken or the batch expires"
	if len(held) > 1 {
		message = "The deployments serving " + ModelsText(held) + " sleep by an operator's or a schedule's order, which requests do not wake; their requests are retried until they are woken or the batch expires"
	}
	return []Warning{{Code: WarningModelSleepHeld, Param: param, Models: held, Message: message}}
}
