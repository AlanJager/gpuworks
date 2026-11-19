package uiassets

import (
	"regexp"
	"strconv"
)

// pathStep is the kind of SVG path command whose end point walkPath reports.
type pathStep byte

const (
	pathMove  pathStep = iota // M, m
	pathLine                  // L, H, V (absolute or relative)
	pathCurve                 // C, S, Q, T, A (absolute or relative)
	pathClose                 // Z, z: the point is the subpath's start
)

var pathTokenRE = regexp.MustCompile(`[A-Za-z]|[-+]?(?:\d+\.?\d*|\.\d+)(?:[eE][-+]?\d+)?`)

// pathCurveArity is how many numbers each curve and arc command takes.
var pathCurveArity = map[byte]int{'c': 6, 's': 4, 'q': 4, 't': 2, 'a': 7}

// walkPath calls visit with the absolute end point of every command in d.
// It returns false when it cannot follow d (an unknown command, a missing
// number, a stray number after Z); arcs written with compressed flags
// ("a1 1 0 01 2 2") are not split and read wrong.
func walkPath(d string, visit func(step pathStep, x, y float64)) bool {
	toks := pathTokenRE.FindAllString(d, -1)
	var (
		x, y, startX, startY float64
		cmd                  byte
	)
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
				return false
			}
			if rel {
				nx, ny = x+nx, y+ny
			}
			x, y = nx, ny
			if cmd == 'M' || cmd == 'm' {
				startX, startY = x, y
				cmd = map[byte]byte{'M': 'L', 'm': 'l'}[cmd] // coordinates after a moveto are linetos
				visit(pathMove, x, y)
			} else {
				visit(pathLine, x, y)
			}
		case 'H', 'h':
			nx, ok := num(&i)
			if !ok {
				return false
			}
			if rel {
				nx += x
			}
			x = nx
			visit(pathLine, x, y)
		case 'V', 'v':
			ny, ok := num(&i)
			if !ok {
				return false
			}
			if rel {
				ny += y
			}
			y = ny
			visit(pathLine, x, y)
		case 'Z', 'z':
			x, y = startX, startY
			visit(pathClose, x, y)
			cmd = 0 // Z takes no numbers; a stray one ends the parse
		default:
			n, ok := pathCurveArity[cmd|0x20]
			if !ok {
				return false
			}
			var args [7]float64
			for k := 0; k < n; k++ {
				if args[k], ok = num(&i); !ok {
					return false
				}
			}
			if rel {
				x, y = x+args[n-2], y+args[n-1]
			} else {
				x, y = args[n-2], args[n-1]
			}
			visit(pathCurve, x, y)
		}
	}
	return true
}

// pathBounds is the box around every end point in d. Curve control points
// and arc bulges are ignored, so a curve that bows past its end points is
// slightly under-measured; weighting colours by area needs no more.
func pathBounds(d string) (svgBox, bool) {
	var b svgBox
	n := 0
	ok := walkPath(d, func(_ pathStep, x, y float64) {
		if n == 0 {
			b = svgBox{x, y, x, y}
		}
		b.x0, b.x1 = min(b.x0, x), max(b.x1, x)
		b.y0, b.y1 = min(b.y0, y), max(b.y1, y)
		n++
	})
	return b, ok && b.x1 > b.x0 && b.y1 > b.y0
}
