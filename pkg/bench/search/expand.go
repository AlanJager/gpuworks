package search

// ExpandJSON converts the compact API search JSON into deterministic points.
// Adaptive modes are handled exclusively by Strategy and never expanded here.
func ExpandJSON(mode string, raw []byte) ([]Point, error) {
	doc, err := ParseDocument(raw)
	if err != nil {
		return nil, err
	}
	return doc.Expand()
}

// Expand returns the grid points of the document in execution order: every
// concurrency (closed loop) or request rate (open loop), then input length,
// then output length.
func (d Document) Expand() ([]Point, error) {
	isl, osl := d.lengths()
	var out []Point
	if d.OpenLoop() {
		for _, rate := range d.RequestRates {
			for _, i := range isl {
				for _, o := range osl {
					out = append(out, Point{Concurrency: d.MaxInFlight, ISL: i, OSL: o, RequestRate: rate})
				}
			}
		}
		return out, nil
	}
	conc, err := d.Concurrencies()
	if err != nil {
		return nil, err
	}
	if len(conc) == 0 {
		conc = []int{1}
	}
	for _, c := range conc {
		for _, i := range isl {
			for _, o := range osl {
				out = append(out, Point{Concurrency: c, ISL: i, OSL: o})
			}
		}
	}
	return out, nil
}
