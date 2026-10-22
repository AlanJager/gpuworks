package modelrepo

import (
	"fmt"
	"html"
	"net/url"
	"strconv"
	"strings"
)

// This file parses the three directory index dialects a plain file server
// emits: Apache mod_autoindex (a <table> row per entry), nginx "autoindex on"
// (a <pre> block, one line per entry) and Caddy "file_server browse" (a
// <table> whose anchor wraps a <span class="name">). All three are read the
// same way: every <a href> is classified, and the text that follows it up to
// the end of its row supplies the size column.
//
// Nothing here trusts the document. A href is accepted only when it resolves,
// after percent decoding, to a direct child of the directory being listed.

// parseDirectoryIndex returns the accepted entries of one directory index.
// baseURL is the listing root, dirURL the directory this document describes,
// and escapedRel that directory's still-encoded path below the root.
func parseDirectoryIndex(document string, baseURL *url.URL, dirURL, escapedRel string) ([]httpDirEntry, error) {
	// Relative hrefs in a directory index resolve against the directory, which
	// is only true of the trailing-slash form of its URL.
	dir, err := url.Parse(dirURL + "/")
	if err != nil {
		return nil, fmt.Errorf("http directory URL %q is not a valid URL: %w", dirURL, err)
	}
	basePrefix := baseURL.EscapedPath() + "/"
	entries := make([]httpDirEntry, 0, 16)
	seen := make(map[string]struct{}, 16)
	for _, anchor := range scanAnchors(document) {
		entry, ok := classifyHref(anchor.href, baseURL, dir, basePrefix, escapedRel)
		if !ok {
			continue
		}
		if _, duplicate := seen[entry.escapedRel]; duplicate {
			continue
		}
		seen[entry.escapedRel] = struct{}{}
		if !entry.isDir {
			entry.size = parseApproxSize(sizeField(anchor.tail))
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// classifyHref decides whether one href names a file or directory inside the
// listing, and rejects everything else: the sort links in an Apache header
// row, the parent-directory link, off-site links, and any spelling that
// escapes the listed root.
func classifyHref(href string, baseURL, dir *url.URL, basePrefix, escapedRel string) (httpDirEntry, bool) {
	href = strings.TrimSpace(href)
	if href == "" || strings.HasPrefix(href, "?") || strings.HasPrefix(href, "#") {
		return httpDirEntry{}, false
	}
	// An absolute path is the parent-directory link or a site-root link; a
	// listing never needs one, so it is never followed.
	if strings.HasPrefix(href, "/") {
		return httpDirEntry{}, false
	}
	// Caddy writes every entry as "./name"; the leading current-directory
	// marker is the only dot segment accepted, and only in that position.
	href = strings.TrimPrefix(href, "./")
	ref, err := url.Parse(href)
	if err != nil {
		return httpDirEntry{}, false
	}
	if ref.RawQuery != "" || ref.ForceQuery || ref.Fragment != "" {
		return httpDirEntry{}, false
	}
	resolved := dir.ResolveReference(ref)
	// An off-site link, or a same-host link that climbed out of the listing
	// root, is not part of this repository.
	if !strings.EqualFold(resolved.Scheme, baseURL.Scheme) || !strings.EqualFold(resolved.Host, baseURL.Host) {
		return httpDirEntry{}, false
	}
	escapedPath := resolved.EscapedPath()
	if !strings.HasPrefix(escapedPath, basePrefix) {
		return httpDirEntry{}, false
	}
	escaped := strings.TrimPrefix(escapedPath, basePrefix)
	isDir := strings.HasSuffix(escaped, "/")
	escaped = strings.Trim(escaped, "/")
	if escaped == "" {
		return httpDirEntry{}, false
	}
	// Decode before checking segments: ResolveReference treats "%2e%2e" as an
	// ordinary segment, so a traversal hidden in percent escapes survives it.
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		return httpDirEntry{}, false
	}
	if strings.ContainsRune(decoded, 0) || strings.Contains(decoded, "\\") {
		return httpDirEntry{}, false
	}
	for _, segment := range strings.Split(decoded, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return httpDirEntry{}, false
		}
	}
	// Only a direct child of the directory being listed is accepted, so a
	// crafted href cannot reach sideways into another repository's tree or
	// skip past the depth budget.
	child := decoded
	if escapedRel != "" {
		prefix, err := url.PathUnescape(escapedRel)
		if err != nil {
			return httpDirEntry{}, false
		}
		if !strings.HasPrefix(decoded, prefix+"/") {
			return httpDirEntry{}, false
		}
		child = strings.TrimPrefix(decoded, prefix+"/")
	}
	if child == "" || strings.Contains(child, "/") {
		return httpDirEntry{}, false
	}
	return httpDirEntry{escapedRel: escaped, rel: decoded, isDir: isDir}, true
}

type anchor struct {
	href string
	// tail is the markup between this anchor's </a> and the end of its row.
	tail string
}

// rowWindow bounds how much markup after an anchor is searched for its row
// end. A directory index row is a few hundred bytes; without the bound a
// listing of thousands of entries would rescan the rest of the document once
// per entry.
const rowWindow = 4096

// scanAnchors walks the document's <a> elements without pulling in an HTML
// parser: a directory index is machine-generated markup, and the only thing
// read from it is the href plus the row text that follows. The whole document
// is traversed once, so a listing with thousands of rows stays linear.
func scanAnchors(document string) []anchor {
	lower := strings.ToLower(document)
	out := make([]anchor, 0, 16)
	preScanned, inPre := 0, false
	for index := 0; index < len(document); {
		start := indexAnchorOpen(lower, index)
		if start < 0 {
			break
		}
		inPre = trackPre(lower[preScanned:start], inPre)
		preScanned = start
		gt := strings.IndexByte(document[start:], '>')
		if gt < 0 {
			break
		}
		openEnd := start + gt + 1
		index = openEnd
		href, ok := attrValue(document[start:openEnd], "href")
		if !ok {
			continue
		}
		tailStart := openEnd
		if closeAt := strings.Index(lower[openEnd:], "</a>"); closeAt >= 0 {
			tailStart = openEnd + closeAt + len("</a>")
			index = tailStart
		}
		out = append(out, anchor{href: html.UnescapeString(href), tail: rowTail(document, lower, tailStart, inPre)})
	}
	return out
}

// trackPre reports whether the document is inside a <pre> block after the
// given chunk, given whether it was inside before it.
func trackPre(chunk string, inPre bool) bool {
	for {
		opened := strings.Index(chunk, "<pre")
		closed := strings.Index(chunk, "</pre>")
		switch {
		case opened < 0 && closed < 0:
			return inPre
		case closed < 0 || (opened >= 0 && opened < closed):
			inPre = true
			chunk = chunk[opened+len("<pre"):]
		default:
			inPre = false
			chunk = chunk[closed+len("</pre>"):]
		}
	}
}

// rowTail returns the markup from the end of an anchor to the end of its row.
// A <pre> listing (nginx) ends a row at the newline; a table listing (Apache,
// Caddy) ends it at </tr>, and Caddy spreads one row over several lines, so
// the newline must not end a row there.
func rowTail(document, lower string, from int, inPre bool) string {
	if from >= len(document) {
		return ""
	}
	to := from + rowWindow
	if to > len(document) {
		to = len(document)
	}
	tail := document[from:to]
	lowerTail := lower[from:to]
	stops := []string{"</tr>", "</li>", "</pre>", "</table>"}
	if inPre {
		stops = append(stops, "\n")
	}
	end := len(tail)
	if next := indexAnchorOpen(lowerTail, 0); next >= 0 && next < end {
		end = next
	}
	for _, stop := range stops {
		if at := strings.Index(lowerTail, stop); at >= 0 && at < end {
			end = at
		}
	}
	return tail[:end]
}

// indexAnchorOpen finds the next "<a" that opens an anchor element rather than
// the start of some other tag such as <address> or <abbr>.
func indexAnchorOpen(lower string, from int) int {
	for from < len(lower) {
		at := strings.Index(lower[from:], "<a")
		if at < 0 {
			return -1
		}
		at += from
		next := at + len("<a")
		if next >= len(lower) {
			return -1
		}
		switch lower[next] {
		case ' ', '\t', '\r', '\n', '>', '/':
			return at
		}
		from = next
	}
	return -1
}

// attrValue reads one attribute out of a start tag. Quoted and bare values are
// both accepted because the three dialects differ on quoting.
func attrValue(tag, name string) (string, bool) {
	lower := strings.ToLower(tag)
	for from := 0; ; {
		at := strings.Index(lower[from:], name)
		if at < 0 {
			return "", false
		}
		at += from
		from = at + len(name)
		// The attribute name must stand alone: "data-href" is not "href".
		if at > 0 {
			previous := lower[at-1]
			if previous != ' ' && previous != '\t' && previous != '\r' && previous != '\n' {
				continue
			}
		}
		rest := strings.TrimLeft(tag[from:], " \t\r\n")
		if !strings.HasPrefix(rest, "=") {
			continue
		}
		rest = strings.TrimLeft(rest[1:], " \t\r\n")
		if rest == "" {
			return "", false
		}
		switch rest[0] {
		case '"', '\'':
			quote := rest[0]
			end := strings.IndexByte(rest[1:], quote)
			if end < 0 {
				return "", false
			}
			return rest[1 : 1+end], true
		default:
			end := strings.IndexAny(rest, " \t\r\n>")
			if end < 0 {
				end = len(rest)
			}
			return strings.TrimSuffix(rest[:end], "/"), true
		}
	}
}

// sizeField extracts the size column out of a row's trailing markup. The three
// dialects put the size in different places relative to the timestamp, and
// Caddy separates the number from its unit, so every token and every adjacent
// pair is tried and the last one that reads as a size wins.
func sizeField(tail string) string {
	fields := strings.Fields(stripTags(tail))
	found := ""
	for index := 0; index < len(fields); index++ {
		if index+1 < len(fields) && isSizeUnit(fields[index+1]) && looksNumeric(fields[index]) {
			found = fields[index] + fields[index+1]
			index++
			continue
		}
		if _, ok := sizeParts(fields[index]); ok {
			found = fields[index]
		}
	}
	return found
}

func stripTags(markup string) string {
	var out strings.Builder
	out.Grow(len(markup))
	depth := 0
	for _, char := range markup {
		switch {
		case char == '<':
			depth++
			out.WriteByte(' ')
		case char == '>' && depth > 0:
			depth--
			out.WriteByte(' ')
		case depth == 0:
			out.WriteRune(char)
		}
	}
	return html.UnescapeString(out.String())
}

func looksNumeric(value string) bool {
	_, err := strconv.ParseFloat(value, 64)
	return err == nil
}

func isSizeUnit(value string) bool {
	_, ok := sizeMultiplier(value)
	return ok && value != ""
}

// parseApproxSize reads one size cell. Directory rows ("-", "&mdash;"), an
// empty cell and a unit this parser does not know all yield 0, meaning
// "unknown": a listing must never be rejected because one row was unreadable,
// and a wrong number would be worse than no number.
//
// Units are binary (1 K = 1024), which is what all three servers print.
func parseApproxSize(value string) int64 {
	parts, ok := sizeParts(value)
	if !ok {
		return 0
	}
	amount, err := strconv.ParseFloat(parts.number, 64)
	if err != nil || amount < 0 {
		return 0
	}
	multiplier, known := sizeMultiplier(parts.unit)
	if !known {
		return 0
	}
	scaled := amount * float64(multiplier)
	if scaled > float64(1<<62) {
		return 0
	}
	return int64(scaled)
}

type sizeCell struct {
	number string
	unit   string
}

func sizeParts(value string) (sizeCell, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return sizeCell{}, false
	}
	end := 0
	dots := 0
	for end < len(value) {
		char := value[end]
		if char >= '0' && char <= '9' {
			end++
			continue
		}
		if char == '.' && dots == 0 && end > 0 {
			dots++
			end++
			continue
		}
		break
	}
	if end == 0 {
		return sizeCell{}, false
	}
	number := strings.TrimSuffix(value[:end], ".")
	unit := strings.TrimSpace(value[end:])
	if _, known := sizeMultiplier(unit); !known {
		return sizeCell{}, false
	}
	return sizeCell{number: number, unit: unit}, true
}

func sizeMultiplier(unit string) (int64, bool) {
	normalized := strings.ToUpper(strings.TrimSpace(unit))
	normalized = strings.TrimSuffix(normalized, "IB")
	if normalized != "B" {
		normalized = strings.TrimSuffix(normalized, "B")
	}
	switch normalized {
	case "", "B":
		return 1, true
	case "K":
		return 1 << 10, true
	case "M":
		return 1 << 20, true
	case "G":
		return 1 << 30, true
	case "T":
		return 1 << 40, true
	case "P":
		return 1 << 50, true
	default:
		return 0, false
	}
}
