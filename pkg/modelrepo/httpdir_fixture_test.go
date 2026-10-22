package modelrepo

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The fixtures below reproduce what the three supported file servers actually
// put on the wire, down to the header sort links, the parent-directory row and
// the size column's spelling. A listing parser is only worth as much as the
// markup it was proven against, so a fixture that "looks close enough" would
// make these tests meaningless.

type indexFlavor string

const (
	flavorApache indexFlavor = "apache"
	flavorNginx  indexFlavor = "nginx"
	flavorCaddy  indexFlavor = "caddy"
)

// indexEntry is one row a fixture server prints. size is the literal text that
// flavor puts in the size column; rawHref replaces the href the server would
// normally emit, which is how the hostile-href cases are injected.
type indexEntry struct {
	name    string
	isDir   bool
	size    string
	rawHref string
}

type rawResponse struct {
	status      int
	contentType string
	body        string
}

type fileServerFixture struct {
	flavor indexFlavor
	root   string
	dirs   map[string][]indexEntry
	files  map[string]string
	raw    map[string]rawResponse

	requests atomic.Int64
	methods  chan string
}

func newFixture(t *testing.T, flavor indexFlavor, root string) *fileServerFixture {
	t.Helper()
	return &fileServerFixture{
		flavor:  flavor,
		root:    root,
		dirs:    map[string][]indexEntry{},
		files:   map[string]string{},
		raw:     map[string]rawResponse{},
		methods: make(chan string, 256),
	}
}

func (f *fileServerFixture) start(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	return server, server.URL + f.root
}

func (f *fileServerFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	select {
	case f.methods <- r.Method + " " + r.URL.Path:
	default:
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !strings.HasPrefix(r.URL.Path, f.root) {
		http.NotFound(w, r)
		return
	}
	rel := strings.Trim(strings.TrimPrefix(r.URL.Path, f.root), "/")
	if response, ok := f.raw[rel]; ok {
		w.Header().Set("Content-Type", response.contentType)
		status := response.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response.body))
		return
	}
	if entries, ok := f.dirs[rel]; ok {
		// A real file server redirects the directory to its trailing-slash
		// form; relative hrefs would resolve one level too high otherwise.
		if !strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
			return
		}
		w.Header().Set("Content-Type", "text/html;charset=UTF-8")
		_, _ = w.Write([]byte(f.render(r.URL.Path, rel, entries)))
		return
	}
	if body, ok := f.files[rel]; ok {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", `"`+rel+`"`)
		http.ServeContent(w, r, rel, time.Unix(1788000000, 0).UTC(), strings.NewReader(body))
		return
	}
	http.NotFound(w, r)
}

func (f *fileServerFixture) href(entry indexEntry) string {
	if entry.rawHref != "" {
		return entry.rawHref
	}
	name := entry.name
	if f.flavor == flavorCaddy {
		name = "./" + name
	}
	if entry.isDir {
		name += "/"
	}
	return name
}

func (f *fileServerFixture) render(urlPath, rel string, entries []indexEntry) string {
	parent := f.root + "/"
	if rel != "" {
		parent = strings.TrimSuffix(urlPath, "/")
		parent = parent[:strings.LastIndex(parent, "/")+1]
	} else if index := strings.LastIndex(strings.TrimSuffix(f.root, "/"), "/"); index >= 0 {
		parent = f.root[:index+1]
	}
	switch f.flavor {
	case flavorNginx:
		return renderNginx(urlPath, entries, f.href)
	case flavorCaddy:
		return renderCaddy(urlPath, entries, f.href)
	default:
		return renderApache(urlPath, parent, entries, f.href)
	}
}

// renderApache reproduces mod_autoindex's FancyIndexing table, including the
// "?C=N;O=D" column-sort links and the absolute Parent Directory href.
func renderApache(urlPath, parent string, entries []indexEntry, href func(indexEntry) string) string {
	var out strings.Builder
	out.WriteString(`<!DOCTYPE HTML PUBLIC "-//W3C//DTD HTML 3.2 Final//EN">` + "\n")
	out.WriteString("<html>\n <head>\n  <title>Index of " + urlPath + "</title>\n </head>\n <body>\n")
	out.WriteString("<h1>Index of " + urlPath + "</h1>\n  <table>\n")
	out.WriteString(`   <tr><th valign="top"><img src="/icons/blank.gif" alt="[ICO]"></th><th><a href="?C=N;O=D">Name</a></th><th><a href="?C=M;O=A">Last modified</a></th><th><a href="?C=S;O=A">Size</a></th><th><a href="?C=D;O=A">Description</a></th></tr>` + "\n")
	out.WriteString(`   <tr><th colspan="5"><hr></th></tr>` + "\n")
	out.WriteString(`<tr><td valign="top"><img src="/icons/back.gif" alt="[PARENTDIR]"></td><td><a href="` + parent + `">Parent Directory</a></td><td>&nbsp;</td><td align="right">  - </td><td>&nbsp;</td></tr>` + "\n")
	for _, entry := range entries {
		icon := "text.gif"
		if entry.isDir {
			icon = "folder.gif"
		}
		label := entry.name
		if entry.isDir {
			label += "/"
		}
		out.WriteString(`<tr><td valign="top"><img src="/icons/` + icon + `" alt="[   ]"></td><td><a href="` + href(entry) + `">` + label + `</a></td><td align="right">2026-09-01 14:43  </td><td align="right">` + entry.size + `  </td><td>&nbsp;</td></tr>` + "\n")
	}
	out.WriteString(`   <tr><th colspan="5"><hr></th></tr>` + "\n</table>\n")
	out.WriteString("<address>Apache/2.4.6 (CentOS) Server at hf-mirror.zstack.ai Port 443</address>\n</body></html>\n")
	return out.String()
}

// renderNginx reproduces "autoindex on": one <pre> block, one line per entry,
// a "../" parent link and exact byte counts right-aligned after the timestamp.
func renderNginx(urlPath string, entries []indexEntry, href func(indexEntry) string) string {
	var out strings.Builder
	out.WriteString("<html>\n<head><title>Index of " + urlPath + "</title></head>\n<body>\n")
	out.WriteString("<h1>Index of " + urlPath + "</h1><hr><pre><a href=\"../\">../</a>\n")
	for _, entry := range entries {
		label := entry.name
		if entry.isDir {
			label += "/"
		}
		padding := 51 - len(label)
		if padding < 1 {
			padding = 1
		}
		size := entry.size
		if entry.isDir {
			size = "-"
		}
		out.WriteString(`<a href="` + href(entry) + `">` + label + "</a>" + strings.Repeat(" ", padding) +
			"01-Sep-2026 14:43" + fmt.Sprintf("%20s", size) + "\n")
	}
	out.WriteString("</pre><hr></body>\n</html>\n")
	return out.String()
}

// renderCaddy reproduces "file_server browse": a table whose anchor wraps a
// <span class="name">, hrefs written as "./name", the size printed as a
// human-readable value split from its unit, and an em dash for directories.
func renderCaddy(urlPath string, entries []indexEntry, href func(indexEntry) string) string {
	var out strings.Builder
	out.WriteString("<!DOCTYPE html>\n<html>\n<head>\n<title>" + urlPath + "</title>\n</head>\n<body>\n")
	out.WriteString("<table aria-describedby=\"summary\">\n<tbody>\n")
	out.WriteString("<tr>\n<td></td>\n<td><a href=\"..\"><span class=\"go-up\">up</span></a></td>\n<td>&mdash;</td>\n<td>&mdash;</td>\n</tr>\n")
	for _, entry := range entries {
		size := entry.size
		if entry.isDir {
			size = "&mdash;"
		}
		out.WriteString("<tr class=\"file\">\n<td></td>\n<td>\n<a href=\"" + href(entry) + "\">\n" +
			"<span class=\"name\">" + entry.name + "</span>\n</a>\n</td>\n" +
			"<td>" + size + "</td>\n<td>09/01/2026 02:43:00 PM +00:00</td>\n</tr>\n")
	}
	out.WriteString("</tbody>\n</table>\n</body>\n</html>\n")
	return out.String()
}

func listFixture(t *testing.T, fixture *fileServerFixture) ([]File, error) {
	t.Helper()
	server, base := fixture.start(t)
	return ListFiles(context.Background(), SourceURL, "Qwen/Qwen3-0.6B", "", base, nil,
		RequestOptions{Client: server.Client()})
}

func fileMap(files []File) map[string]int64 {
	out := make(map[string]int64, len(files))
	for _, file := range files {
		out[file.Path] = file.Size
	}
	return out
}

// qwen3Entries is the real /Qwen/Qwen3-0.6B listing: eight files, no
// subdirectories, sizes as mod_autoindex prints them.
func qwen3Entries() []indexEntry {
	return []indexEntry{
		{name: "config.json", size: "726"},
		{name: "configuration.json", size: "73"},
		{name: "generation_config.json", size: "239"},
		{name: "merges.txt", size: "1.6M"},
		{name: "model.safetensors", size: "1.4G"},
		{name: "tokenizer.json", size: "11M"},
		{name: "tokenizer_config.json", size: "9.5K"},
		{name: "vocab.json", size: "2.6M"},
	}
}
