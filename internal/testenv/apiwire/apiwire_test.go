package apiwire

import (
	"encoding/json"
	"strings"
	"testing"
)

type row struct {
	Name string `json:"name"`
}

func TestListDecodesTheEnvelopeAndRejectsEveryOtherShape(t *testing.T) {
	var got []row
	if err := UnmarshalList([]byte(`{"items":[{"name":"a"},{"name":"b"}],"total":2}`), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Fatalf("items = %+v", got)
	}
	if err := UnmarshalList([]byte(`{"items":[],"total":0}`), &got); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty list = %#v err=%v, want a non-nil empty slice", got, err)
	}
	for name, body := range map[string]string{
		"bare array":     `[{"name":"a"}]`,
		"null body":      `null`,
		"missing total":  `{"items":[]}`,
		"missing items":  `{"total":0}`,
		"null items":     `{"items":null,"total":0}`,
		"total mismatch": `{"items":[{"name":"a"}],"total":2}`,
		"extra key":      `{"items":[],"total":0,"page":1}`,
		"wrong item":     `{"items":[1],"total":1}`,
	} {
		if err := UnmarshalList([]byte(body), &got); err == nil {
			t.Errorf("%s: %s decoded, want an error", name, body)
		}
	}
}

func TestListIsUsableAsADecodeTarget(t *testing.T) {
	var list List[row]
	if err := json.NewDecoder(strings.NewReader(`{"items":[{"name":"a"}],"total":1}`)).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if list.Total != 1 || len(list.Items) != 1 || list.Items[0].Name != "a" {
		t.Fatalf("list = %+v", list)
	}
	if err := json.Unmarshal([]byte(`[{"name":"a"}]`), &list); err == nil {
		t.Fatal("a bare array decoded into List")
	}
}

func TestCursorPageHasNoTotal(t *testing.T) {
	var got []row
	if err := UnmarshalCursorPage([]byte(`{"items":[{"name":"a"}]}`), &got); err != nil || len(got) != 1 {
		t.Fatalf("items = %+v err=%v", got, err)
	}
	for _, body := range []string{`[{"name":"a"}]`, `{"items":[],"total":0}`, `{"items":null}`, `{}`} {
		if err := UnmarshalCursorPage([]byte(body), &got); err == nil {
			t.Errorf("%s decoded as a cursor page", body)
		}
	}
}

func TestMetricSeriesPointsArePairs(t *testing.T) {
	var series []MetricSeries
	if err := UnmarshalList([]byte(`{"items":[{"labels":{"node":"n1"},"points":[[1700000000000,2.5],[1700000010000,3]]}],"total":1}`), &series); err != nil {
		t.Fatal(err)
	}
	if len(series) != 1 || series[0].Labels["node"] != "n1" || len(series[0].Points) != 2 {
		t.Fatalf("series = %+v", series)
	}
	if p := series[0].Points[0]; p.TimeMS != 1700000000000 || p.Value != 2.5 {
		t.Fatalf("point = %+v", p)
	}
	for _, body := range []string{
		`{"items":[{"labels":{},"points":[{"TimeMS":1,"Value":2}]}],"total":1}`,
		`{"items":[{"labels":{},"points":[[1]]}],"total":1}`,
		`{"items":[{"labels":{},"points":[[1,2,3]]}],"total":1}`,
		`{"items":[{"labels":{},"points":[[1.5,2]]}],"total":1}`,
	} {
		if err := UnmarshalList([]byte(body), &series); err == nil {
			t.Errorf("%s decoded", body)
		}
	}
}

func TestLogsPageAndWindow(t *testing.T) {
	var page LogsPage
	if err := json.Unmarshal([]byte(`{"items":[{"id":7,"instance":"i","node":"n","level":"error","source":"instance","time_ms":5,"line":"boom"}],"total":1,"truncated":true}`), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].TimeMS != 5 || page.Items[0].Line != "boom" || page.Total != 1 || page.TotalCapped || !page.Truncated {
		t.Fatalf("page = %+v", page)
	}
	// total/total_capped/truncated are omitempty on the wire.
	if err := json.Unmarshal([]byte(`{"items":[]}`), &page); err != nil || page.Total != 0 || page.Truncated {
		t.Fatalf("minimal page = %+v err=%v", page, err)
	}
	for _, body := range []string{`{"Entries":[],"Total":0}`, `[]`, `{"items":[],"Truncated":true}`} {
		if err := json.Unmarshal([]byte(body), &page); err == nil {
			t.Errorf("%s decoded as a logs page", body)
		}
	}
	var window LogsWindow
	if err := json.Unmarshal([]byte(`{"items":[{"time_ms":1,"line":"a"}],"match_index":0}`), &window); err != nil || len(window.Items) != 1 {
		t.Fatalf("window = %+v err=%v", window, err)
	}
	if err := json.Unmarshal([]byte(`{"Entries":[],"MatchIndex":0}`), &window); err == nil {
		t.Error("the pre-rename context shape decoded")
	}
}
