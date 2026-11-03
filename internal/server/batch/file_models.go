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
// request goes to the model its line names, so a model no deployment serves,
// or one whose deployments do not expose the batch's endpoint, would fail all
// of its requests at once. Such a file is refused; models that wait for
// someone to act (ModelState.Waits) are only warned about, as an override
// naming one is.
func (s *Service) checkFileModels(f File, endpoint string) ([]Warning, error) {
	if s.cfg.Models == nil {
		return nil, nil
	}
	names, err := s.fileModels(f)
	if err != nil {
		return nil, err
	}
	c, err := s.classify(names, endpoint)
	if err != nil {
		return nil, err
	}
	if len(c.unknown) > 0 {
		e := invalid(CodeModelNotFound, ParamInputFileID, "The input file names models that no deployment serves: "+ModelsText(c.unknown)+
			". Set a model to send every request to, or deploy these models first")
		e.Models = c.unknown
		return nil, e
	}
	if len(c.unserved) > 0 {
		e := invalid(CodeModelEndpoint, ParamInputFileID, "The input file names models whose deployments do not serve "+endpoint+": "+ModelsText(c.unserved)+
			". Set a model that serves it, or use the endpoint these models serve")
		e.Models, e.Endpoint = c.unserved, endpoint
		return nil, e
	}
	return c.warnings(ParamInputFileID), nil
}

// classified is what looking a batch's models up found, each list in the order
// of the names looked up.
type classified struct {
	// unknown no deployment serves; unserved are served without the batch's
	// endpoint.
	unknown, unserved []string
	// waiting are the names whose requests wait for someone to act, by state
	// (ModelState.Waits).
	waiting map[ModelState][]string
}

// classify looks names up for a batch of endpoint. It stops once
// maxReportedModels are refused.
func (s *Service) classify(names []string, endpoint string) (classified, error) {
	c := classified{waiting: map[ModelState][]string{}}
	for _, name := range names {
		entry, err := s.cfg.Models.Lookup(name)
		if err != nil {
			return classified{}, err
		}
		switch {
		case !entry.State.Known():
			c.unknown = append(c.unknown, name)
		case !entry.Serves(endpoint):
			c.unserved = append(c.unserved, name)
		case entry.State.Waits():
			c.waiting[entry.State] = append(c.waiting[entry.State], name)
		}
		if len(c.unknown)+len(c.unserved) >= maxReportedModels {
			break
		}
	}
	return c, nil
}

// warnings are the warnings about the names that wait, one per state in the
// order of ModelStates; param is the request field that chose them.
func (c classified) warnings(param string) []Warning {
	var out []Warning
	for _, state := range ModelStates {
		names := c.waiting[state]
		if len(names) == 0 {
			continue
		}
		if state == ModelSleepHeld {
			out = append(out, heldWarning(param, names))
			continue
		}
		out = append(out, Warning{Code: WarningModelWaits, Param: param, Models: names, State: state,
			Message: "The deployments serving " + ModelsText(names) + " are " + waitWords[state] + "; their requests are retried until they serve or the batch expires"})
	}
	return out
}

// waitWords say in English what keeps a waiting model from serving.
var waitWords = map[ModelState]string{
	ModelOffline: "on nodes that are not connected",
	ModelFailed:  "failed",
	ModelStopped: "stopped",
}

// heldWarning is the warning about models whose deployments sleep by an
// operator's or a schedule's order; param is the request field that chose
// them.
func heldWarning(param string, held []string) Warning {
	message := "The deployment serving " + held[0] + " sleeps by an operator's or a schedule's order, which requests do not wake; the batch's requests are retried until it is woken or the batch expires"
	if len(held) > 1 {
		message = "The deployments serving " + ModelsText(held) + " sleep by an operator's or a schedule's order, which requests do not wake; their requests are retried until they are woken or the batch expires"
	}
	return Warning{Code: WarningModelSleepHeld, Param: param, Models: held, Message: message}
}
