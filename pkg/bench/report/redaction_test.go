package report

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
)

func TestContextExcludesImportedBodiesEvenWhenDetailIsDisabled(t *testing.T) {
	run := Run{KeepDetail: false, Dataset: []byte(`{"kind":"imported","seed":42,"samples":[{"messages":[{"role":"user","content":"PRIVATE_PROMPT_9347"}],"follow_ups":[{"role":"user","content":"PRIVATE_FOLLOWUP_8256"}],"input_tokens":100,"output_tokens":20}]}`), Points: []Point{{Seq: 1, Success: 1, OutputTput: 1, DetailStatus: "disabled"}}}
	for _, format := range []string{"csv", "html", "markdown"} {
		doc, err := Render(format, run)
		if err != nil {
			t.Fatal(err)
		}
		body := string(doc.Body)
		if strings.Contains(body, "PRIVATE_PROMPT_9347") || strings.Contains(body, "PRIVATE_FOLLOWUP_8256") {
			t.Fatalf("%s leaked request body", format)
		}
	}
	row := csvPoint(t, run)
	var context struct {
		Dataset struct {
			Kind    string          `json:"kind"`
			Samples json.RawMessage `json:"samples"`
			Profile struct {
				SampleCount    int `json:"sample_count"`
				MaxInputTokens int `json:"max_input_tokens"`
			} `json:"profile"`
		} `json:"dataset"`
	}
	if err := json.Unmarshal([]byte(row["report_context_json"]), &context); err != nil || context.Dataset.Samples != nil || context.Dataset.Profile.SampleCount != 1 || context.Dataset.Profile.MaxInputTokens != 100 {
		t.Fatalf("safe dataset lost profile: %s err=%v", row["report_context_json"], err)
	}
}

func TestHistoricalDetailErrorCredentialsAreRedactedWithoutMutatingCaller(t *testing.T) {
	record := timedRecord(time.Unix(100, 0), "historical-session", 1)
	record.ErrorText = `Post "https://HISTORY_USER:HISTORY_PASSWORD@host.test/v1?token=HISTORY_TOKEN&safe=yes": connection refused`
	record.ErrorStage = "transport"
	record.Text = "explicitly retained body"
	run := Run{Points: []Point{{Seq: 1, Success: 1, Details: []loadgen.DetailRecord{record}}}}
	for _, format := range []string{"html", "markdown", "csv"} {
		document, err := Render(format, run)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"HISTORY_USER", "HISTORY_PASSWORD", "HISTORY_TOKEN"} {
			if strings.Contains(string(document.Body), secret) {
				t.Errorf("%s retained historical detail credential %s", format, secret)
			}
		}
		if format != "csv" && (!strings.Contains(string(document.Body), "[diagnostic suffix omitted]") || !strings.Contains(string(document.Body), "explicitly retained body")) {
			t.Errorf("%s lost historical omission marker or explicitly retained body", format)
		}
		if run.Points[0].Details[0].ErrorText != record.ErrorText || run.Points[0].Details[0].Text != record.Text {
			t.Fatal("render mutated caller detail record")
		}
	}
}

func TestHistoricalMalformedURLCredentialsDoNotLeakFromReport(t *testing.T) {
	for _, password := range []string{`BAD"SECRET`, "BAD`SECRET", `BAD'"SECRET`, `BAD\"SECRET`} {
		_, parseError := http.NewRequest(http.MethodPost, "https://USER_545:"+password+"@host.test/v1", nil)
		if parseError == nil {
			t.Fatal("fixture should be a real malformed URL")
		}
		record := timedRecord(time.Unix(100, 0), "historical-malformed", 1)
		record.ErrorText = parseError.Error()
		run := Run{Points: []Point{{Seq: 1, Details: []loadgen.DetailRecord{record}}}}
		for _, format := range []string{"html", "markdown"} {
			doc, err := Render(format, run)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"USER_545", "BAD", "SECRET"} {
				if strings.Contains(string(doc.Body), secret) {
					t.Errorf("%s leaked malformed URL credential fragment %s", format, secret)
				}
			}
			if !strings.Contains(string(doc.Body), "[diagnostic suffix omitted]") || run.Points[0].Details[0].ErrorText != parseError.Error() {
				t.Fatal("historical omission not explicit or caller mutated")
			}
		}
	}
}

func TestMalformedRawSnapshotURLsDoNotLeakCredentialSuffixes(t *testing.T) {
	for _, password := range []string{`BAD"SECRET`, "BAD`SECRET", `BAD'"SECRET`, `BAD\"SECRET`, "BAD SECRET"} {
		endpoint := "https://USER_545:" + password + "@host.test/v1?api_key=QUERY_545&safe=yes"
		snapshot, err := json.Marshal(map[string]any{"base_url": endpoint, "env": map[string]string{"HTTP_PROXY": endpoint}, "argv": []string{"--endpoint", endpoint}})
		if err != nil {
			t.Fatal(err)
		}
		for _, format := range []string{"html", "markdown", "csv"} {
			doc, err := Render(format, Run{TargetSnapshot: snapshot, Points: []Point{{Seq: 1}}})
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"USER_545", "BAD", "SECRET", "QUERY_545"} {
				if strings.Contains(string(doc.Body), secret) {
					t.Errorf("%s leaked raw malformed URL fragment %s", format, secret)
				}
			}
		}
	}
}

func TestRawSnapshotAuthQueryWithSpacesIsRedacted(t *testing.T) {
	snapshot := []byte(`{"base_url":"https://host.test/v1?authorization=Bearer SECRET_545&safe=yes","env":{"HTTP_PROXY":"https://proxy.test/v1?auth=Bearer SECRET_545&safe=yes"},"argv":["--endpoint","https://host.test/v1?authorization=Bearer SECRET_545&safe=yes"]}`)
	for _, format := range []string{"html", "markdown", "csv"} {
		doc, err := Render(format, Run{TargetSnapshot: snapshot, Points: []Point{{Seq: 1}}})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(doc.Body), "SECRET_545") || strings.Contains(string(doc.Body), "Bearer") {
			t.Errorf("%s leaked literal-space query credential", format)
		}
		if !strings.Contains(string(doc.Body), "safe=yes") || !strings.Contains(string(doc.Body), "host.test") {
			t.Errorf("%s lost safe endpoint evidence", format)
		}
	}
}

func TestSnapshotPreservesBenignPercentValues(t *testing.T) {
	snapshot := []byte(`{"env":{"NORMAL":"50%","FORMAT":"%s"},"argv":["--ratio","50%","--format=%s","https://USER_545:BAD\"SECRET_545@host.test/v1?token=QUERY_545"]}`)
	for _, format := range []string{"html", "markdown", "csv"} {
		doc, err := Render(format, Run{TargetSnapshot: snapshot, Points: []Point{{Seq: 1}}})
		if err != nil {
			t.Fatal(err)
		}
		body := string(doc.Body)
		for _, literal := range []string{"50%", "%s", "--format=%s"} {
			if !strings.Contains(body, literal) {
				t.Errorf("%s changed benign literal %q", format, literal)
			}
		}
		for _, secret := range []string{"USER_545", "BAD", "SECRET_545", "QUERY_545"} {
			if strings.Contains(body, secret) {
				t.Errorf("%s preserved malformed URL credential", format)
			}
		}
	}
}

func TestHistoricalAmbiguousURLDiagnosticsAreOmittedWithoutLeakingSuffixes(t *testing.T) {
	for _, message := range []string{
		"endpoint rejected https://host.test/v1?authorization=Bearer SPACE_SECRET_545&safe=yes",
		"failed https://USER_545:BAD SECRET_545@host.test/v1",
		"failed https://USER_545:BAD\nSECRET_545@host.test/v1",
		"error https://safe.test/?trace=yes https://USER_545:BAD_SECRET_545@host.test/v1",
		"error https://safe.test/?next=https://USER_545:BAD_SECRET_545@host.test/v1",
		"error https://safe.test/?next=https%3A%2F%2FUSER_545%3ABAD_SECRET_545%40host.test%2Fv1",
		"error 'https://USER_545:BAD'SECRET_545@host.test/v1'",
		"error `https://USER_545:BAD`SECRET_545@host.test/v1`",
		`error "https://host.test/v1?token=BAD"SECRET_545"`,
		`error "https://USER_545:BAD": SECRET_545@host.test/v1"`,
		`error "https://host.test/v1?token=BAD": SECRET_545"`,
		`error "https://USER_545:BAD": SECRET_545@host.test/v1`,
		`error "https://host.test/v1?token=BAD": SECRET_545`,
	} {
		record := timedRecord(time.Unix(100, 0), "historical-ambiguous", 1)
		record.ErrorText = message
		run := Run{Points: []Point{{Seq: 1, Details: []loadgen.DetailRecord{record}}}}
		for _, format := range []string{"html", "markdown", "csv"} {
			doc, err := Render(format, run)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"USER_545", "BAD", "SECRET_545"} {
				if strings.Contains(string(doc.Body), secret) {
					t.Errorf("%s retained unknown diagnostic credential %s", format, secret)
				}
			}
			if run.Points[0].Details[0].ErrorText != message {
				t.Fatal("historical diagnostic was mutated")
			}
		}
	}
}

func TestTypedTransportDiagnosticKeepsSafeEndpointAndReasonThroughReport(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	endpoint := strings.Replace(server.URL, "://", "://REPORT_USER_545:REPORT_PASSWORD_545@", 1) + "?token=REPORT_TOKEN_545&safe=yes"
	result, err := (loadgen.Client{BaseURL: endpoint}).Do(context.Background(), loadgen.Request{})
	if err == nil || result.Metrics.ErrorStage != "transport" {
		t.Fatal("expected real transport failure")
	}
	record := loadgen.NewDetailRecord(result)
	record.SessionID, record.Turn = "typed-transport", 1
	for _, format := range []string{"html", "markdown"} {
		doc, err := Render(format, Run{Points: []Point{{Seq: 1, Details: []loadgen.DetailRecord{record}}}})
		if err != nil {
			t.Fatal(err)
		}
		body := string(doc.Body)
		for _, secret := range []string{"REPORT_USER_545", "REPORT_PASSWORD_545", "REPORT_TOKEN_545"} {
			if strings.Contains(body, secret) {
				t.Errorf("%s retained transport credential", format)
			}
		}
		if !strings.Contains(body, "connection refused") || !strings.Contains(body, "safe=yes") || strings.Contains(body, "[diagnostic suffix omitted]") {
			t.Errorf("%s lost structured transport diagnosis", format)
		}
	}
}

func TestAllReportFormatsRedactSnapshotCredentials(t *testing.T) {
	run := Run{TargetSnapshot: []byte(`{"target_kind":"external","base_url":"http://user:URL_PASSWORD_94@example.test:3000/v1?api_key=QUERY_KEY_81&key=GENERIC_QUERY_KEY_32&safe=yes","entrypoint":["engine","--api-key","ENTRYPOINT_KEY_14","sh -c \"exec engine --token SHELL_SECRET_01\""],"argv":["--password=ARGV_PASSWORD_31","--endpoint","https://user:ARGV_URL_PASSWORD_44@remote.test/v1?token=ARGV_QUERY_KEY_55","--header=Authorization: Bearer HEADER_KEY_23","--model","public-model"],"docker_args":["--env","HF_TOKEN=ENV_TOKEN_21","-e","NORMAL=visible","--env=API_KEY=INLINE_KEY_88"],"env":{"AWS_ACCESS_KEY_ID":"ACCESS_KEY_35","PRIVATE_KEY":"PRIVATE_KEY_26","HF_TOKEN":"ENV_KEY_78","NORMAL":"visible","HTTP_PROXY":"http://user:PROXY_PASSWORD_18@proxy.test:8080"}}`), Points: []Point{{Seq: 1, Success: 1, OutputTput: 1}}}
	for _, format := range []string{"markdown", "html", "csv"} {
		doc, err := Render(format, run)
		if err != nil {
			t.Fatal(err)
		}
		body := string(doc.Body)
		for _, secret := range []string{"URL_PASSWORD_94", "QUERY_KEY_81", "ENTRYPOINT_KEY_14", "ARGV_PASSWORD_31", "ARGV_URL_PASSWORD_44", "ARGV_QUERY_KEY_55", "HEADER_KEY_23", "ENV_TOKEN_21", "INLINE_KEY_88", "ENV_KEY_78", "PROXY_PASSWORD_18", "SHELL_SECRET_01", "ACCESS_KEY_35", "PRIVATE_KEY_26", "GENERIC_QUERY_KEY_32"} {
			if strings.Contains(body, secret) {
				t.Errorf("%s leaked %s", format, secret)
			}
		}
		for _, safe := range []string{"public-model", "visible", "example.test", "safe=yes", "remote.test", "proxy.test", "REDACTED"} {
			if !strings.Contains(body, safe) {
				t.Errorf("%s lost non-secret evidence %s", format, safe)
			}
		}
	}
	// The CSV still obeys the normal escaping contract after URL/argv redaction.
	doc, _ := Render("csv", run)
	if _, err := csv.NewReader(strings.NewReader(string(doc.Body))).ReadAll(); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotEnvironmentValuesRedactBundledCredentials(t *testing.T) {
	snapshot := []byte(`{"env":{"ENGINE_ARGS":"--api-key=REVIEW_ENV_VALUE_545_A","ENGINE_CONFIG":"{\"authorization\":\"Bearer REVIEW_ENV_VALUE_545_B\"}","FLAGS":"--model public-model --token REVIEW_ENV_VALUE_545_C","NORMAL":"50%","FORMAT":"%s"}}`)
	for _, format := range []string{"html", "markdown", "csv"} {
		doc, err := Render(format, Run{TargetSnapshot: snapshot, Points: []Point{{Seq: 1}}})
		if err != nil {
			t.Fatal(err)
		}
		body := string(doc.Body)
		if strings.Contains(body, "REVIEW_ENV_VALUE_545_") {
			t.Errorf("%s retained a credential bundled inside an environment value", format)
		}
		for _, safe := range []string{"50%", "%s"} {
			if !strings.Contains(body, safe) {
				t.Errorf("%s lost benign format value %q", format, safe)
			}
		}
	}
}

func TestSnapshotRedactsNestedCredentialValuesAndShellArguments(t *testing.T) {
	config := `{"client":{"headers":{"x-api-key":"REVIEW_NESTED_545_A"}}}`
	escaped, _ := json.Marshal(config)
	benign := `{"client":{"headers":{"accept":"application/json"}},"ratio":"50%","format":"%s","args":"--format=%s"}`
	snapshot, _ := json.Marshal(map[string]any{
		"env":  map[string]string{"ENGINE_CONFIG": config, "ARRAY_CONFIG": `[{"client":{"password":"REVIEW_NESTED_545_B"}}]`, "ESCAPED_CONFIG": string(escaped), "BENIGN_CONFIG": benign},
		"argv": []string{config, `exec${IFS}engine${IFS}--api-key${IFS}REVIEW_NESTED_545_C`},
	})
	for _, format := range []string{"html", "markdown", "csv"} {
		doc, err := Render(format, Run{TargetSnapshot: snapshot, Points: []Point{{Seq: 1}}})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(doc.Body), "REVIEW_NESTED_545_") {
			t.Errorf("%s retained nested or shell credential", format)
		}
	}
	var safe struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(sanitizedSnapshot(snapshot), &safe); err != nil {
		t.Fatal(err)
	}
	if safe.Env["BENIGN_CONFIG"] != benign {
		t.Fatalf("benign JSON changed: %q", safe.Env["BENIGN_CONFIG"])
	}
}

func TestHistoricalSnapshotURLFragmentsAreNotExported(t *testing.T) {
	for _, fragment := range []string{"access_token=REVIEW_FRAGMENT_545_A", "access_token=REVIEW%5fFRAGMENT%5f545%5fB", "next=https%3A%2F%2Fhost.test%2F%3Ftoken%3DREVIEW_FRAGMENT_545_C"} {
		snapshot, _ := json.Marshal(map[string]any{"base_url": "https://host.test/v1?safe=yes#" + fragment})
		for _, format := range []string{"html", "markdown", "csv"} {
			doc, err := Render(format, Run{TargetSnapshot: snapshot, Points: []Point{{Seq: 1}}})
			if err != nil {
				t.Fatal(err)
			}
			body := string(doc.Body)
			if strings.Contains(body, "FRAGMENT") || strings.Contains(body, "access_token") || strings.Contains(body, "next=") {
				t.Errorf("%s retained URL fragment", format)
			}
			if !strings.Contains(body, "safe=yes") {
				t.Errorf("%s lost safe query", format)
			}
		}
	}
}
