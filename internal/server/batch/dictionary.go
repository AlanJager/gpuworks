package batch

import "time"

// Dictionary is what a client needs to offer a batch: the endpoints, the
// completion windows and file limits the create checks, the statuses a batch
// moves through, how long its results are kept, and the served names it can be
// sent to. A client shows these and decides none of them.
type Dictionary struct {
	Endpoints []string
	// MinWindowHours, MaxWindowHours and DefaultWindowHours bound the
	// completion window (ParseWindow) and give the one a batch gets unasked.
	MinWindowHours, MaxWindowHours, DefaultWindowHours int
	MaxFileBytes                                       int64
	MaxFileLines                                       int
	Statuses                                           []string
	OutputRetention                                    time.Duration
	// Models are Models.List: the names the create check accepts or warns
	// about, with their state. Empty without a Models.
	Models []ModelEntry
}

// Dictionary is the service's dictionary, the models read now.
func (s *Service) Dictionary() (Dictionary, error) {
	d := Dictionary{
		Endpoints:      append([]string(nil), Endpoints...),
		MinWindowHours: MinWindowHours, MaxWindowHours: MaxWindowHours, DefaultWindowHours: DefaultWindowHours,
		MaxFileBytes: MaxFileBytes, MaxFileLines: MaxFileLines,
		Statuses:        append([]string(nil), Statuses...),
		OutputRetention: s.cfg.OutputRetention,
		Models:          []ModelEntry{},
	}
	if s.cfg.Models == nil {
		return d, nil
	}
	models, err := s.cfg.Models.List()
	if err != nil {
		return Dictionary{}, err
	}
	if models != nil {
		d.Models = models
	}
	return d, nil
}
