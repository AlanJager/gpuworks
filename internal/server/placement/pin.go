package placement

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.starlark.net/syntax"
)

// PinCount is a resolver that accepts only count cards on one node and
// otherwise answers what source does. An empty source accepts that shape with
// no variables. The guard is the first statement of source's resolve(gpu), so
// every answer source gives on the pinned shape is unchanged; reason is what
// the guard rejects any other shape with.
func PinCount(source string, count int, reason string) (string, error) {
	guard := func(indent string) string {
		return indent + "if gpu.count != " + strconv.Itoa(count) + " or gpu.nnodes != 1:\n" +
			indent + "    return reject(" + strconv.Quote(reason) + ")\n"
	}
	if strings.TrimSpace(source) == "" {
		return "def resolve(gpu):\n" + guard("    ") + "    return {}\n", nil
	}
	if err := validateProgram(source); err != nil {
		return "", err
	}
	file, err := syntax.LegacyFileOptions().Parse("placement.star", source, 0)
	if err != nil {
		return "", err
	}
	definition := file.Stmts[0].(*syntax.DefStmt)
	start, _ := definition.Span()
	first, _ := definition.Body[0].Span()
	lines := strings.SplitAfter(source, "\n")
	if first.Line == start.Line || int(first.Line) > len(lines) {
		return "", errors.New("placement resolver body starts on the def line")
	}
	line := lines[first.Line-1]
	indent := line[:min(int(first.Col)-1, len(line))]
	if strings.TrimLeft(indent, " \t") != "" {
		return "", fmt.Errorf("placement resolver body at line %d does not start a line", first.Line)
	}
	lines[first.Line-1] = guard(indent) + line
	pinned := strings.Join(lines, "")
	if err := Compile(pinned); err != nil {
		return "", err
	}
	return pinned, nil
}
