package uiassets

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Accelerator vendor marks sit side by side on light and dark surfaces, so
// every mark must be artwork on a transparent canvas: SVG (preferred) or PNG
// with an alpha channel, never a box with a painted background. These tests
// are the rule; docs/dev/ui.md ("Vendor marks") describes it for people.

var (
	uiRoot         = filepath.Join("..", "..", "ui")
	vendorLogoDir  = filepath.Join(uiRoot, "public", "vendor-logos")
	vendorAssetsTS = filepath.Join(uiRoot, "src", "components", "vendorAssets.ts")
)

const (
	// A painted SVG shape covering this share of the viewBox is a background.
	svgBackgroundCoverage = 0.90
	// At least this share of a raster's 1-px border must be fully transparent.
	rasterMinTransparentBorder = 0.50
	// A raster with this share of opaque pixels or more is a box, not a mark.
	// Shipped marks measure 10-31%; a plate behind the artwork pushes it up.
	rasterMaxOpaqueShare = 0.60
	// A pixel at or above this alpha (out of 65535, about 240/255) counts as
	// opaque, so a plate exported at 254/255 is caught like one at 255.
	rasterOpaqueAlpha = 0xF000
)

const (
	fixRaster = "fix: re-export the mark with a transparent background (PNG with an alpha channel, nothing painted behind the artwork), or ship it as SVG"
	fixSVG    = "fix: remove the background shape or style so only the artwork is painted, and keep the canvas transparent"
)

// TestVendorLogosHaveTransparentBackgrounds checks every shipped mark.
func TestVendorLogosHaveTransparentBackgrounds(t *testing.T) {
	entries, err := os.ReadDir(vendorLogoDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatalf("%s holds no vendor marks", vendorLogoDir)
	}
	for _, e := range entries {
		if e.IsDir() {
			t.Errorf("ui/public/vendor-logos/%s: a directory; vendor marks are flat files; fix: move the mark up into ui/public/vendor-logos/", e.Name())
			continue
		}
		data, err := os.ReadFile(filepath.Join(vendorLogoDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range checkVendorLogo("ui/public/vendor-logos/"+e.Name(), data) {
			t.Error(p)
		}
	}
}

// TestVendorAssetsMatchLogoFiles keeps the console's map and the directory in
// step: every mapped path exists and every file is mapped.
func TestVendorAssetsMatchLogoFiles(t *testing.T) {
	src, err := os.ReadFile(vendorAssetsTS)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := vendorAssetRefs(src)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(vendorLogoDir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]bool{}
	for _, e := range entries {
		files[e.Name()] = true
	}
	for _, p := range vendorAssetProblems(refs, files) {
		t.Error(p)
	}
}

func vendorAssetRefs(src []byte) (map[string]string, error) {
	block := regexp.MustCompile(`(?s)export const VENDOR_ASSETS\b[^=]*=\s*\{(.*?)\n\};`).FindSubmatch(src)
	if block == nil {
		return nil, errors.New("ui/src/components/vendorAssets.ts declares no VENDOR_ASSETS map")
	}
	refs := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^\s*"?([A-Za-z0-9_-]+)"?\s*:\s*"([^"]*)"`).FindAllSubmatch(block[1], -1) {
		refs[string(m[1])] = string(m[2])
	}
	if len(refs) == 0 {
		return nil, errors.New("ui/src/components/vendorAssets.ts: VENDOR_ASSETS has no entries")
	}
	return refs, nil
}

func vendorAssetProblems(refs map[string]string, files map[string]bool) []string {
	var problems []string
	used := map[string]bool{}
	keys := make([]string, 0, len(refs))
	for k := range refs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ref := refs[k]
		name := strings.TrimPrefix(ref, "/vendor-logos/")
		if name == ref || strings.Contains(name, "/") {
			problems = append(problems, fmt.Sprintf("ui/src/components/vendorAssets.ts: VENDOR_ASSETS.%s = %q is not a file directly under /vendor-logos/; fix: put the mark in ui/public/vendor-logos/ and point the entry at /vendor-logos/<file>", k, ref))
			continue
		}
		used[name] = true
		if !files[name] {
			problems = append(problems, fmt.Sprintf("ui/src/components/vendorAssets.ts: VENDOR_ASSETS.%s points at %s, which does not exist; fix: add ui/public/vendor-logos/%s or correct the path", k, ref, name))
		}
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if !used[n] {
			problems = append(problems, fmt.Sprintf("ui/public/vendor-logos/%s: not referenced by VENDOR_ASSETS in ui/src/components/vendorAssets.ts; fix: map it to its vendor or delete the file", n))
		}
	}
	return problems
}

// checkVendorLogo returns one self-contained message per broken rule.
func checkVendorLogo(name string, data []byte) []string {
	switch ext := strings.ToLower(path.Ext(name)); ext {
	case ".svg":
		return checkSVGLogo(name, data)
	case ".png":
		return checkPNGLogo(name, data)
	default:
		return []string{fmt.Sprintf("%s: vendor marks must be .svg or .png with a transparent background, %q is not allowed (JPEG, GIF and BMP have no usable alpha channel; WebP is not checked); fix: export the mark as SVG (preferred) or as PNG with an alpha channel and nothing painted behind the artwork", name, ext)}
	}
}

func checkPNGLogo(name string, data []byte) []string {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return []string{fmt.Sprintf("%s: not a decodable PNG (%v); fix: re-export it as a real PNG with an alpha channel", name, err)}
	}
	if !hasAlphaChannel(img) {
		return []string{fmt.Sprintf("%s: PNG has no alpha channel (colour model %T), so its background is opaque; %s", name, img.ColorModel(), fixRaster)}
	}
	b := img.Bounds()
	alpha := func(x, y int) uint32 { _, _, _, a := img.At(x, y).RGBA(); return a }
	var problems []string
	for _, c := range [][2]int{{b.Min.X, b.Min.Y}, {b.Max.X - 1, b.Min.Y}, {b.Min.X, b.Max.Y - 1}, {b.Max.X - 1, b.Max.Y - 1}} {
		if a := alpha(c[0], c[1]); a != 0 {
			problems = append(problems, fmt.Sprintf("%s: corner pixel (%d,%d) has alpha %d/255, but all four corners must be fully transparent; %s", name, c[0], c[1], a>>8, fixRaster))
		}
	}
	ring, clear := 0, 0
	opaque := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			a := alpha(x, y)
			if a >= rasterOpaqueAlpha {
				opaque++
			}
			if x == b.Min.X || y == b.Min.Y || x == b.Max.X-1 || y == b.Max.Y-1 {
				ring++
				if a == 0 {
					clear++
				}
			}
		}
	}
	if ring > 0 && float64(clear)/float64(ring) < rasterMinTransparentBorder {
		problems = append(problems, fmt.Sprintf("%s: only %.0f%% of the 1-px border is fully transparent, at least %.0f%% must be (a painted box or padding plate surrounds the mark); %s", name, 100*float64(clear)/float64(ring), 100*rasterMinTransparentBorder, fixRaster))
	}
	if total := b.Dx() * b.Dy(); total > 0 && float64(opaque)/float64(total) >= rasterMaxOpaqueShare {
		problems = append(problems, fmt.Sprintf("%s: %.0f%% of pixels are opaque (alpha 240/255 or more), it must be under %.0f%% (the image is a filled box, not a mark on a transparent canvas); %s", name, 100*float64(opaque)/float64(total), 100*rasterMaxOpaqueShare, fixRaster))
	}
	return problems
}

func hasAlphaChannel(img image.Image) bool {
	switch m := img.ColorModel(); m {
	case color.NRGBAModel, color.NRGBA64Model, color.RGBAModel, color.RGBA64Model, color.AlphaModel, color.Alpha16Model:
		return true
	default:
		if p, ok := m.(color.Palette); ok { // tRNS on a palette image
			for _, c := range p {
				if _, _, _, a := c.RGBA(); a != 0xffff {
					return true
				}
			}
		}
		return false
	}
}

type svgPaint struct {
	fill    string
	opacity float64 // product of opacity and fill-opacity down the tree
}

type svgBox struct{ x0, y0, x1, y1 float64 }

func checkSVGLogo(name string, data []byte) []string {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var (
		problems []string
		stack    []svgPaint
		viewBox  *svgBox
		depth    int
		inStyle  bool
		// depth of the outermost element whose children are never drawn
		// directly (defs, clipPath, ...); 0 when outside all of them
		hiddenAt int
	)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return append(problems, fmt.Sprintf("%s: not well-formed XML (%v); fix: re-export the SVG", name, err))
		}
		switch el := tok.(type) {
		case xml.StartElement:
			depth++
			attrs := map[string]string{}
			for _, a := range el.Attr {
				attrs[a.Name.Local] = a.Value
			}
			style := parseStyle(attrs["style"])
			tag := el.Name.Local
			if depth == 1 {
				if tag != "svg" {
					return []string{fmt.Sprintf("%s: root element is <%s>, not <svg>; fix: re-export the mark as a plain SVG", name, tag)}
				}
				vb, ok := parseViewBox(attrs["viewBox"])
				if !ok {
					problems = append(problems, fmt.Sprintf("%s: root <svg> has no valid viewBox, so the mark cannot scale to the logo box; fix: add viewBox=\"0 0 <width> <height>\" cropped tight to the artwork", name))
				} else {
					viewBox = &vb
				}
			}
			if _, ok := style["background"]; ok {
				problems = append(problems, fmt.Sprintf("%s: <%s> sets a CSS background in its style attribute, which paints a box behind the mark; %s", name, tag, fixSVG))
			} else if _, ok := style["background-color"]; ok {
				problems = append(problems, fmt.Sprintf("%s: <%s> sets a CSS background-color in its style attribute, which paints a box behind the mark; %s", name, tag, fixSVG))
			}
			if _, ok := attrs["background"]; ok {
				problems = append(problems, fmt.Sprintf("%s: <%s> has a background attribute; %s", name, tag, fixSVG))
			}
			switch tag {
			case "image":
				problems = append(problems, fmt.Sprintf("%s: embeds an <image>, whose raster background the check cannot see; fix: redraw the mark as vector paths, or ship it as a transparent PNG instead", name))
			case "foreignObject":
				problems = append(problems, fmt.Sprintf("%s: contains a <foreignObject>, whose HTML content the check cannot see; fix: redraw the mark as vector paths", name))
			case "style":
				inStyle = true
			}
			if hiddenAt == 0 && svgUndrawnContainers[tag] {
				hiddenAt = depth
			}
			paint := svgPaint{fill: "black", opacity: 1}
			if len(stack) > 0 {
				paint = stack[len(stack)-1]
			}
			if v, ok := pick(attrs, style, "fill"); ok && v != "inherit" {
				paint.fill = v
			}
			for _, k := range []string{"opacity", "fill-opacity"} {
				if v, ok := pick(attrs, style, k); ok {
					if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
						paint.opacity *= f
					}
				}
			}
			stack = append(stack, paint)
			if viewBox != nil && hiddenAt == 0 && painted(paint) {
				if box, ok := shapeBox(tag, attrs, *viewBox); ok {
					if c := coverage(box, *viewBox); c >= svgBackgroundCoverage {
						problems = append(problems, fmt.Sprintf("%s: <%s> with fill %q covers %.0f%% of the viewBox (limit %.0f%%), so it is a background plate, not artwork; %s", name, tag, paint.fill, 100*c, 100*svgBackgroundCoverage, fixSVG))
					}
				}
			}
		case xml.EndElement:
			if hiddenAt == depth {
				hiddenAt = 0
			}
			depth--
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			if el.Name.Local == "style" {
				inStyle = false
			}
		case xml.CharData:
			if inStyle && strings.Contains(strings.ToLower(string(el)), "background") {
				problems = append(problems, fmt.Sprintf("%s: a <style> block sets a CSS background, which paints a box behind the mark; %s", name, fixSVG))
			}
		}
	}
	if depth != 0 || viewBox == nil && len(problems) == 0 {
		problems = append(problems, fmt.Sprintf("%s: no root <svg> element; fix: re-export the mark as a plain SVG", name))
	}
	return problems
}

func parseStyle(s string) map[string]string {
	out := map[string]string{}
	for _, decl := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(decl, ":")
		if ok {
			out[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	return out
}

func pick(attrs, style map[string]string, key string) (string, bool) {
	if v, ok := style[key]; ok {
		return v, true
	}
	v, ok := attrs[key]
	return v, ok
}

var zeroAlphaColour = regexp.MustCompile(`^(rgba|hsla)\(.*[,/ ]\s*0*(\.0*)?%?\s*\)$`)

func painted(p svgPaint) bool {
	f := strings.ToLower(strings.TrimSpace(p.fill))
	if f == "" || f == "none" || f == "transparent" || p.opacity <= 0 {
		return false
	}
	if zeroAlphaColour.MatchString(f) {
		return false
	}
	// #RGBA / #RRGGBBAA with a zero alpha digit pair.
	if strings.HasPrefix(f, "#") && (len(f) == 5 && f[4] == '0' || len(f) == 9 && f[7:] == "00") {
		return false
	}
	return true
}

func parseViewBox(s string) (svgBox, bool) {
	n := numbers(s)
	if len(n) != 4 || n[2] <= 0 || n[3] <= 0 {
		return svgBox{}, false
	}
	return svgBox{n[0], n[1], n[0] + n[2], n[1] + n[3]}, true
}

var numberRE = regexp.MustCompile(`[-+]?(?:\d+\.?\d*|\.\d+)(?:[eE][-+]?\d+)?`)

func numbers(s string) []float64 {
	var out []float64
	for _, m := range numberRE.FindAllString(s, -1) {
		f, err := strconv.ParseFloat(m, 64)
		if err == nil {
			out = append(out, f)
		}
	}
	return out
}

// length reads an SVG length; a percentage is a share of span.
func length(s string, span float64) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if strings.HasSuffix(s, "%") {
		f, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
		return f / 100 * span, err == nil
	}
	f, err := strconv.ParseFloat(strings.TrimSuffix(s, "px"), 64)
	return f, err == nil
}

// svgUndrawnContainers hold shapes that are only drawn when referenced
// (a clip rect, a gradient stop, a symbol), so their shapes are not measured.
var svgUndrawnContainers = map[string]bool{
	"defs": true, "clipPath": true, "mask": true, "symbol": true,
	"pattern": true, "marker": true, "linearGradient": true, "radialGradient": true,
}

// shapeBox is the bounding box of the shapes that can paint a background.
// A path counts only when one of its subpaths is an axis-aligned rectangle:
// a viewBox cropped tight to a wordmark makes the wordmark's own path span
// it, so general path bounds cannot tell artwork from a plate, but a
// rectangle drawn with M/L/H/V/Z is how editors write a background.
// Transforms, strokes, <use> references and fills set from <style> classes
// are not evaluated.
func shapeBox(tag string, a map[string]string, vb svgBox) (svgBox, bool) {
	w, h := vb.x1-vb.x0, vb.y1-vb.y0
	get := func(k string, span float64) float64 { v, _ := length(a[k], span); return v }
	switch tag {
	case "rect":
		rw, okw := length(a["width"], w)
		rh, okh := length(a["height"], h)
		if !okw || !okh {
			return svgBox{}, false
		}
		x, y := get("x", w), get("y", h)
		return svgBox{x, y, x + rw, y + rh}, true
	case "circle":
		r := get("r", (w+h)/2)
		cx, cy := get("cx", w), get("cy", h)
		return svgBox{cx - r, cy - r, cx + r, cy + r}, r > 0
	case "ellipse":
		rx, ry := get("rx", w), get("ry", h)
		cx, cy := get("cx", w), get("cy", h)
		return svgBox{cx - rx, cy - ry, cx + rx, cy + ry}, rx > 0 && ry > 0
	case "polygon", "polyline":
		n := numbers(a["points"])
		if len(n) < 4 {
			return svgBox{}, false
		}
		b := svgBox{n[0], n[1], n[0], n[1]}
		for i := 0; i+1 < len(n); i += 2 {
			b.x0, b.x1 = min(b.x0, n[i]), max(b.x1, n[i])
			b.y0, b.y1 = min(b.y0, n[i+1]), max(b.y1, n[i+1])
		}
		return b, true
	case "path":
		return pathRectangle(a["d"])
	}
	return svgBox{}, false
}

var pathTokenRE = regexp.MustCompile(`[A-Za-z]|[-+]?(?:\d+\.?\d*|\.\d+)(?:[eE][-+]?\d+)?`)

// pathCurveArity is how many numbers each curve and arc command takes.
var pathCurveArity = map[byte]int{'c': 6, 's': 4, 'q': 4, 't': 2, 'a': 7}

// pathRectangle returns the largest subpath of d that is an axis-aligned
// rectangle: built only from M, L, H, V and Z (absolute or relative), with
// every vertex on a corner of the subpath's own bounds and no diagonal edge.
// A subpath with a curve or arc is artwork; so is a whole path the parser
// cannot follow (compressed arc flags, unknown commands).
func pathRectangle(d string) (svgBox, bool) {
	toks := pathTokenRE.FindAllString(d, -1)
	var (
		best     svgBox
		found    bool
		x, y     float64
		startX   float64
		startY   float64
		pts      [][2]float64
		cmd      byte
		bestArea float64
	)
	curved := false // the current subpath has a curve or arc
	flush := func() {
		if len(pts) < 4 || curved {
			pts, curved = pts[:0], false
			return
		}
		b := svgBox{pts[0][0], pts[0][1], pts[0][0], pts[0][1]}
		for _, p := range pts {
			b.x0, b.x1 = min(b.x0, p[0]), max(b.x1, p[0])
			b.y0, b.y1 = min(b.y0, p[1]), max(b.y1, p[1])
		}
		rect := b.x1 > b.x0 && b.y1 > b.y0
		for i, p := range pts {
			q := pts[(i+1)%len(pts)] // closing edge included
			if (p[0] != b.x0 && p[0] != b.x1) || (p[1] != b.y0 && p[1] != b.y1) || (p[0] != q[0] && p[1] != q[1]) {
				rect = false // a vertex off a corner, or a diagonal edge
				break
			}
		}
		if area := (b.x1 - b.x0) * (b.y1 - b.y0); rect && area > bestArea {
			best, bestArea, found = b, area, true
		}
		pts = pts[:0]
	}
	num := func(i *int) (float64, bool) {
		if *i >= len(toks) {
			return 0, false
		}
		f, err := strconv.ParseFloat(toks[*i], 64)
		if err != nil {
			return 0, false
		}
		*i++
		return f, true
	}
	for i := 0; i < len(toks); {
		if c := toks[i][0]; (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
			cmd = c
			i++
		}
		rel := cmd >= 'a'
		switch cmd {
		case 'M', 'm', 'L', 'l':
			nx, ok1 := num(&i)
			ny, ok2 := num(&i)
			if !ok1 || !ok2 {
				return svgBox{}, false
			}
			if rel {
				nx, ny = x+nx, y+ny
			}
			if cmd == 'M' || cmd == 'm' {
				flush()
				startX, startY = nx, ny
				cmd = map[byte]byte{'M': 'L', 'm': 'l'}[cmd] // coordinates after a moveto are linetos
			}
			x, y = nx, ny
			pts = append(pts, [2]float64{x, y})
		case 'H', 'h':
			nx, ok := num(&i)
			if !ok {
				return svgBox{}, false
			}
			if rel {
				nx += x
			}
			x = nx
			pts = append(pts, [2]float64{x, y})
		case 'V', 'v':
			ny, ok := num(&i)
			if !ok {
				return svgBox{}, false
			}
			if rel {
				ny += y
			}
			y = ny
			pts = append(pts, [2]float64{x, y})
		case 'Z', 'z':
			x, y = startX, startY
			flush()
			pts = append(pts, [2]float64{x, y}) // a lineto after Z starts here
			cmd = 0                             // Z takes no numbers; a stray one ends the parse
		default:
			n, ok := pathCurveArity[cmd|0x20]
			if !ok {
				return svgBox{}, false
			}
			var args [7]float64
			for k := 0; k < n; k++ {
				if args[k], ok = num(&i); !ok {
					return svgBox{}, false
				}
			}
			if rel {
				x, y = x+args[n-2], y+args[n-1]
			} else {
				x, y = args[n-2], args[n-1]
			}
			curved = true
			pts = append(pts, [2]float64{x, y})
		}
	}
	flush()
	return best, found
}

func coverage(b, vb svgBox) float64 {
	iw := min(b.x1, vb.x1) - max(b.x0, vb.x0)
	ih := min(b.y1, vb.y1) - max(b.y0, vb.y0)
	if iw <= 0 || ih <= 0 {
		return 0
	}
	return iw * ih / ((vb.x1 - vb.x0) * (vb.y1 - vb.y0))
}
