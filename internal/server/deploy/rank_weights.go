// rank_weights.go owns rank-scoped weights (ADR 0076): a multi-node spec whose
// weight source names the rank in its subfolder, so each rank of a group reads
// only its own part of one repository.

package deploy

import (
	"regexp"
	"strconv"
	"strings"
)

// rankVariables are the spellings of the rank variable a weight subfolder may
// use: the bare launch variable and its documented prefixed form, the same two
// a template's args accept (placement.Canonical).
var rankVariables = []string{"${NODE_RANK}", "${GPUWORKS_NODE_RANK}"}

var subfolderVariable = regexp.MustCompile(`\$\{[^}]*\}`)

// RankScopedSubfolder reports whether a weight source's subfolder names the
// rank, as in "rank${NODE_RANK}".
func RankScopedSubfolder(subfolder string) bool {
	for _, variable := range rankVariables {
		if strings.Contains(subfolder, variable) {
			return true
		}
	}
	return false
}

// UnknownSubfolderVariable returns the first ${...} in subfolder that is not
// the rank variable, "" when there is none. The rank is the only fact a
// subfolder may depend on: it is known before anything is downloaded.
func UnknownSubfolderVariable(subfolder string) string {
	for _, match := range subfolderVariable.FindAllString(subfolder, -1) {
		known := false
		for _, variable := range rankVariables {
			known = known || match == variable
		}
		if !known {
			return match
		}
	}
	return ""
}

// ExpandRankSubfolder is the subfolder rank reads. A subfolder that does not
// name the rank is returned unchanged.
func ExpandRankSubfolder(subfolder string, rank int) string {
	for _, variable := range rankVariables {
		subfolder = strings.ReplaceAll(subfolder, variable, strconv.Itoa(rank))
	}
	return subfolder
}

// RankSubfolderMatches reports whether actual is the subfolder some rank of the
// rank-scoped template reads ("rank${NODE_RANK}" matches "rank3", not
// "rank3/x" or the whole repository).
func RankSubfolderMatches(template, actual string) bool {
	if !RankScopedSubfolder(template) {
		return template == actual
	}
	_, ok := RankOfSubfolder(template, actual)
	return ok
}

// RankOfSubfolder is the rank whose expansion of template is actual.
func RankOfSubfolder(template, actual string) (int, bool) {
	pattern := regexp.QuoteMeta(template)
	for _, variable := range rankVariables {
		pattern = strings.ReplaceAll(pattern, regexp.QuoteMeta(variable), `([0-9]+)`)
	}
	match := regexp.MustCompile(`^` + pattern + `$`).FindStringSubmatch(actual)
	if match == nil {
		return 0, false
	}
	rank, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, false
	}
	for _, other := range match[2:] {
		if other != match[1] {
			return 0, false
		}
	}
	return rank, ExpandRankSubfolder(template, rank) == actual
}

// RankWeight binds one rank of a deployment with rank-scoped weights to the
// model artifact it mounts: the template's subfolder expanded for the rank,
// resolved once at create like a component's StoreModelID and never again from
// the mutable catalog.
type RankWeight struct {
	Rank         int    `json:"rank"`
	Subfolder    string `json:"subfolder"`
	StoreModelID uint   `json:"store_model_id"`
}

// RankWeightModelID is the model rank mounts in place of the deployment's own,
// and false when the selection's weights are not rank-scoped or bind no such
// rank.
func (s CatalogSelection) RankWeightModelID(rank int) (uint, bool) {
	for _, weight := range s.RankWeights {
		if weight.Rank == rank && weight.StoreModelID != 0 {
			return weight.StoreModelID, true
		}
	}
	return 0, false
}

// RankScoped reports whether the selection's weights are rank-scoped.
func (s CatalogSelection) RankScoped() bool { return len(s.RankWeights) > 0 }
