package uiassets

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// transparentMark is a 40x10 canvas, fully transparent apart from a dark bar.
func transparentMark() *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, 40, 10))
	for y := 3; y < 7; y++ {
		for x := 4; x < 36; x++ {
			img.SetNRGBA(x, y, color.NRGBA{0x10, 0x10, 0x10, 0xff})
		}
	}
	return img
}

func filled(img *image.NRGBA, c color.NRGBA) *image.NRGBA {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

// goodSVG is a wordmark cropped tight to its viewBox: its curved path spans
// the whole canvas, which must not read as a plate.
const goodSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 20"><title>X</title><g fill="#1d1d1f"><path d="M0 0H100V20C50 10 50 10 0 20Z"/><path d="M0 0L100 20H0V0Z"/><rect x="10" y="5" width="20" height="10"/></g></svg>`

// TestVendorLogoCheckRejectsOpaqueArtwork: each fixture breaks exactly the
// rule its message must name, and the message says how to fix it.
func TestVendorLogoCheckRejectsOpaqueArtwork(t *testing.T) {
	var jpg bytes.Buffer
	if err := jpeg.Encode(&jpg, transparentMark(), nil); err != nil {
		t.Fatal(err)
	}
	white := color.NRGBA{0xff, 0xff, 0xff, 0xff}
	opaqueRGBA := image.NewRGBA(image.Rect(0, 0, 40, 10))
	for i := range opaqueRGBA.Pix {
		opaqueRGBA.Pix[i] = 0xff
	}
	gray := image.NewGray(image.Rect(0, 0, 40, 10))
	padded := transparentMark()
	for x := 0; x < 40; x++ { // a plate along top and bottom edges; corners stay clear
		for _, y := range []int{0, 9} {
			if x > 0 && x < 39 {
				padded.SetNRGBA(x, y, white)
			}
		}
	}
	for y := 1; y < 9; y++ {
		padded.SetNRGBA(0, y, white)
		padded.SetNRGBA(39, y, white)
	}
	// a white plate inside a 1-px fully transparent rim: corners and border
	// pass, only the opaque share catches it
	boxed := image.NewNRGBA(image.Rect(0, 0, 40, 10))
	for y := 1; y < 9; y++ {
		for x := 1; x < 39; x++ {
			boxed.SetNRGBA(x, y, white)
		}
	}
	// the same box exported at alpha 254/255 instead of 255
	nearlyOpaque := image.NewNRGBA(boxed.Bounds())
	copy(nearlyOpaque.Pix, boxed.Pix)
	for i := 3; i < len(nearlyOpaque.Pix); i += 4 {
		if nearlyOpaque.Pix[i] == 0xff {
			nearlyOpaque.Pix[i] = 0xfe
		}
	}
	gray16 := image.NewGray16(image.Rect(0, 0, 40, 10))

	cases := []struct {
		name, file string
		data       []byte
		want       []string
	}{
		{"jpeg", "x.jpg", jpg.Bytes(), []string{"x.jpg", `".jpg" is not allowed`, "fix:"}},
		{"gif", "x.gif", []byte("GIF89a"), []string{`".gif" is not allowed`}},
		{"webp", "x.webp", []byte("RIFF....WEBP"), []string{`".webp" is not allowed`}},
		{"png named as png but not png", "x.png", jpg.Bytes(), []string{"not a decodable PNG"}},
		{"opaque white png", "white.png", encodePNG(t, filled(image.NewNRGBA(image.Rect(0, 0, 40, 10)), white)), []string{"white.png", "corner pixel (0,0)", "1-px border", "100% of pixels are opaque", "transparent background"}},
		{"png without alpha", "gray.png", encodePNG(t, gray), []string{"no alpha channel"}},
		{"rgba png fully opaque", "rgba.png", encodePNG(t, opaqueRGBA), []string{"corner pixel"}},
		{"padding plate", "plate.png", encodePNG(t, padded), []string{"plate.png", "of the 1-px border is fully transparent"}},
		{"filled box with clear rim", "box.png", encodePNG(t, boxed), []string{"box.png", "are opaque (alpha 240/255 or more), it must be under 60%"}},
		{"nearly opaque box with clear rim", "box254.png", encodePNG(t, nearlyOpaque), []string{"box254.png", "are opaque"}},
		{"16-bit png without alpha", "gray16.png", encodePNG(t, gray16), []string{"no alpha channel"}},
		{"corner alpha reads in 0-255", "corner.png", encodePNG(t, filled(image.NewNRGBA(image.Rect(0, 0, 40, 10)), color.NRGBA{0, 0, 0, 0x80})), []string{"corner pixel (0,0) has alpha 128/255"}},
		{"svg full-size white rect", "r.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 20"><rect width="100%" height="100%" fill="#fff"/><path d="M1 1h5"/></svg>`), []string{"r.svg", "<rect>", `"#fff"`, "background plate", "remove the background"}},
		{"svg rect with default black fill", "b.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 20"><rect x="-5" y="0" width="110" height="20"/></svg>`), []string{`"black"`, "covers 100%"}},
		{"svg rect inheriting fill from group", "g.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 20"><g style="fill:white"><rect width="95" height="20"/></g></svg>`), []string{`"white"`, "covers 95%"}},
		{"svg absolute rectangle path", "pa.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 20"><path fill="#fff" d="M0 0H100V20H0Z"/><path d="M10 5c1 1 2 2 3 3"/></svg>`), []string{"pa.svg", "<path>", `"#fff"`, "covers 100%"}},
		{"svg relative rectangle path", "pr.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 20"><path fill="#fff" d="m0,0h100v20h-100z"/></svg>`), []string{"<path>", "covers 100%"}},
		{"svg rectangle path with lineto and implicit repeats", "pl.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 20"><path fill="#eee" d="M0 0 100 0 L100 20 0 20z"/></svg>`), []string{"<path>"}},
		{"svg plate with knocked-out letters", "pk.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 20"><path fill-rule="evenodd" d="M0 0h100v20H0zM10 5c1 1 2 2 3 3z"/></svg>`), []string{"<path>", `"black"`}},
		{"svg foreignObject", "fo.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><foreignObject width="10" height="10"><div/></foreignObject></svg>`), []string{"<foreignObject>"}},
		{"svg circle plate", "c.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20"><circle cx="10" cy="10" r="10" fill="red"/></svg>`), []string{"<circle>"}},
		{"svg polygon plate", "p.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><polygon points="0,0 10,0 10,10 0,10" fill="#eee"/></svg>`), []string{"<polygon>"}},
		{"svg style background on root", "s.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10" style="background-color: #fff"><path d="M1 1h5"/></svg>`), []string{"s.svg", "<svg> sets a CSS background-color"}},
		{"svg style background on child", "s2.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><g style="background:#000"><path d="M1 1h5"/></g></svg>`), []string{"<g> sets a CSS background"}},
		{"svg style block background", "s3.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><style>svg{background:#fff}</style><path d="M1 1h5"/></svg>`), []string{"<style> block sets a CSS background"}},
		{"svg embedded raster", "i.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><image href="data:image/png;base64,AAAA" width="5" height="5"/></svg>`), []string{"embeds an <image>"}},
		{"svg without viewBox", "v.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"><path d="M1 1h5"/></svg>`), []string{"v.svg", "no valid viewBox", `viewBox="0 0`}},
		{"not svg", "n.svg", []byte(`<html/>`), []string{"root element is <html>"}},
		{"broken xml", "x.svg", []byte(`<svg viewBox="0 0 1 1"><path></svg>`), []string{"not well-formed XML"}},
		{"empty svg", "e.svg", nil, []string{"no root <svg>"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			problems := checkVendorLogo(tc.file, tc.data)
			if len(problems) == 0 {
				t.Fatalf("%s passed, want rejection", tc.file)
			}
			joined := strings.Join(problems, "\n")
			for _, w := range tc.want {
				if !strings.Contains(joined, w) {
					t.Errorf("problems for %s lack %q:\n%s", tc.file, w, joined)
				}
			}
			for _, p := range problems {
				if !strings.HasPrefix(p, tc.file+": ") || !strings.Contains(p, "fix:") {
					t.Errorf("message must start with the file and say how to fix: %q", p)
				}
			}
		})
	}
}

func TestVendorLogoCheckAcceptsTransparentArtwork(t *testing.T) {
	palette := image.NewPaletted(image.Rect(0, 0, 40, 10), color.Palette{color.NRGBA{}, color.NRGBA{0, 0, 0, 0xff}})
	palette.SetColorIndex(20, 5, 1)
	cases := map[string][]byte{
		"mark.png":    encodePNG(t, transparentMark()),
		"palette.png": encodePNG(t, palette),
		"mark.svg":    []byte(goodSVG),
		"none.svg":    []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect width="10" height="10" fill="none" stroke="#000"/><rect width="10" height="10" fill="#fff" fill-opacity="0"/><rect width="10" height="10" fill="rgba(255,255,255,0)"/><rect width="10" height="10" fill="#ffffff00"/><circle cx="5" cy="5" r="2"/></svg>`),
		"upper.SVG":   []byte(goodSVG),
		"clip.svg":    []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><defs><clipPath id="c"><rect width="10" height="10"/></clipPath><linearGradient id="g"><stop/></linearGradient></defs><mask id="m"><rect width="10" height="10" fill="#fff"/></mask><symbol id="s"><path d="M0 0h10v10H0z"/></symbol><g clip-path="url(#c)"><circle cx="5" cy="5" r="2"/></g><rect width="10" height="10" fill="none"/></svg>`),
	}
	for name, data := range cases {
		if p := checkVendorLogo(name, data); len(p) != 0 {
			t.Errorf("%s rejected:\n%s", name, strings.Join(p, "\n"))
		}
	}
}

func TestVendorAssetMapProblems(t *testing.T) {
	src := []byte(`export const VENDOR_ASSETS: Record<string, string> = {
  nvidia: "/vendor-logos/nvidia.svg",
  // comment
  amd: "/vendor-logos/missing.svg",
  odd: "/elsewhere/odd.svg",
};

export const VENDOR_LOGO_HEIGHT: Record<string, number> = {
  nvidia: 15,
};
`)
	refs, err := vendorAssetRefs(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 3 || refs["nvidia"] != "/vendor-logos/nvidia.svg" {
		t.Fatalf("refs = %v", refs)
	}
	got := strings.Join(vendorAssetProblems(refs, map[string]bool{"nvidia.svg": true, "orphan.png": true}), "\n")
	for _, w := range []string{
		"VENDOR_ASSETS.amd points at /vendor-logos/missing.svg, which does not exist",
		`VENDOR_ASSETS.odd = "/elsewhere/odd.svg" is not a file directly under /vendor-logos/`,
		"ui/public/vendor-logos/orphan.png: not referenced by VENDOR_ASSETS",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("problems lack %q:\n%s", w, got)
		}
	}
	if strings.Contains(got, "nvidia") {
		t.Errorf("a mapped, present file was reported:\n%s", got)
	}
	if _, err := vendorAssetRefs([]byte("export const OTHER = {};")); err == nil {
		t.Error("source without VENDOR_ASSETS accepted")
	}
}
