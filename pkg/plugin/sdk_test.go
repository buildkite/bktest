package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type stubHandler struct{}

func (stubHandler) Configure(req ConfigureRequest) (ConfigureResponse, error) {
	if req.Config.TestCommand == "bad" {
		return ConfigureResponse{}, errors.New("invalid test command")
	}
	return ConfigureResponse{
		RunnerID: "stub",
		Name:     "Stub",
		Config:   RunnerSettings{TestFilePattern: "**/*_test.txt"},
		Features: SupportedFeatures{AutoRetry: true, SplitByExample: true},
		Capabilities: Capabilities{
			DiscoverTargets:  true,
			DiscoverExamples: true,
			InterpretAttempt: true,
			ParseBatchReport: true,
		},
	}, nil
}

func (stubHandler) DiscoverTargets(req DiscoverTargetsRequest) (DiscoverTargetsResponse, error) {
	return DiscoverTargetsResponse{Targets: []string{"a_test.txt", "b_test.txt"}}, nil
}

func (stubHandler) DiscoverExamples(req DiscoverExamplesRequest) (DiscoverExamplesResponse, error) {
	return DiscoverExamplesResponse{
		Examples: []TestCase{{Format: "example", Identifier: "ex-1", Path: req.Files[0]}},
	}, nil
}

func (stubHandler) PrepareAttempt(req PrepareAttemptRequest) (PrepareAttemptResponse, error) {
	return PrepareAttemptResponse{
		Command: "echo",
		Args:    PathsFromTestCases(req.TestCases),
		State:   json.RawMessage(`{"attempt":1}`),
	}, nil
}

func (stubHandler) InterpretAttempt(req InterpretAttemptRequest) (InterpretAttemptResponse, error) {
	return InterpretAttemptResponse{
		Results: []AttemptTestOutcome{{
			TestCase: TestCase{Identifier: "t1", Name: "passes", Path: "a_test.txt"},
			Status:   "passed",
		}},
		State: req.State,
	}, nil
}

func (stubHandler) ParseBatchReport(req ParseBatchReportRequest) (ParseBatchReportResponse, error) {
	return ParseBatchReportResponse{
		Tests: []BatchReportedTest{{
			TestCase: TestCase{Identifier: "t1", Path: "a_test.txt"},
			Status:   "passed",
		}},
	}, nil
}

func TestRunAllOperationsAndErrorEnvelope(t *testing.T) {
	h := stubHandler{}

	t.Run("configure", func(t *testing.T) {
		var out bytes.Buffer
		err := Run(h, []string{"configure"}, strings.NewReader(`{"protocol_version":1,"config":{}}`), &out, &bytes.Buffer{})
		if err != nil {
			t.Fatalf("Run(configure) error = %v", err)
		}
		var resp ConfigureResponse
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		if resp.ProtocolVersion != ProtocolVersion || resp.Name != "Stub" || !resp.Capabilities.DiscoverExamples {
			t.Fatalf("unexpected response: %+v", resp)
		}
	})

	t.Run("configure error envelope", func(t *testing.T) {
		var out bytes.Buffer
		err := Run(h, []string{"configure"}, strings.NewReader(`{"protocol_version":1,"config":{"test_command":"bad"}}`), &out, &bytes.Buffer{})
		if err == nil {
			t.Fatal("expected error from Run(configure)")
		}
		var env struct {
			ProtocolVersion int    `json:"protocol_version"`
			Error           string `json:"error"`
		}
		if uerr := json.Unmarshal(out.Bytes(), &env); uerr != nil {
			t.Fatalf("expected JSON error envelope on stdout, got %q (%v)", out.String(), uerr)
		}
		if env.ProtocolVersion != ProtocolVersion || env.Error != "invalid test command" {
			t.Fatalf("unexpected error envelope: %+v", env)
		}
	})

	t.Run("discover-targets", func(t *testing.T) {
		var out bytes.Buffer
		err := Run(h, []string{"discover-targets"}, strings.NewReader(`{"protocol_version":1}`), &out, &bytes.Buffer{})
		if err != nil {
			t.Fatalf("Run(discover-targets) error = %v", err)
		}
		var resp DiscoverTargetsResponse
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(resp.Targets, []string{"a_test.txt", "b_test.txt"}) {
			t.Fatalf("unexpected targets: %v", resp.Targets)
		}
	})

	t.Run("discover-examples", func(t *testing.T) {
		var out bytes.Buffer
		err := Run(h, []string{"discover-examples"}, strings.NewReader(`{"protocol_version":1,"files":["a_test.txt"]}`), &out, &bytes.Buffer{})
		if err != nil {
			t.Fatalf("Run(discover-examples) error = %v", err)
		}
		var resp DiscoverExamplesResponse
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Examples) != 1 || resp.Examples[0].Identifier != "ex-1" {
			t.Fatalf("unexpected examples: %+v", resp.Examples)
		}
	})

	t.Run("prepare-attempt and interpret-attempt preserve opaque state", func(t *testing.T) {
		var prepOut bytes.Buffer
		if err := Run(h, []string{"prepare-attempt"}, strings.NewReader(`{"protocol_version":1,"test_cases":[{"format":"file","path":"a_test.txt"}]}`), &prepOut, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		var prepResp PrepareAttemptResponse
		if err := json.Unmarshal(prepOut.Bytes(), &prepResp); err != nil {
			t.Fatal(err)
		}
		if prepResp.Command != "echo" || !slices.Equal(prepResp.Args, []string{"a_test.txt"}) {
			t.Fatalf("unexpected prepare response: %+v", prepResp)
		}

		var interpOut bytes.Buffer
		interpReq, _ := json.Marshal(InterpretAttemptRequest{ProtocolVersion: 1, State: prepResp.State})
		if err := Run(h, []string{"interpret-attempt"}, bytes.NewReader(interpReq), &interpOut, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		var interpResp InterpretAttemptResponse
		if err := json.Unmarshal(interpOut.Bytes(), &interpResp); err != nil {
			t.Fatal(err)
		}
		if string(interpResp.State) != `{"attempt":1}` || len(interpResp.Results) != 1 {
			t.Fatalf("unexpected interpret response: %+v", interpResp)
		}
	})

	t.Run("parse-batch-report", func(t *testing.T) {
		var out bytes.Buffer
		if err := Run(h, []string{"parse-batch-report"}, strings.NewReader(`{"protocol_version":1,"format":"stub","report_base64":"e30="}`), &out, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		var resp ParseBatchReportResponse
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Tests) != 1 || resp.Tests[0].Status != "passed" {
			t.Fatalf("unexpected batch report response: %+v", resp)
		}
	})
}

func TestHelpers(t *testing.T) {
	dir := t.TempDir()
	for _, rel := range []string{"spec/a_spec.rb", "spec/skip/b_spec.rb", "node_modules/c_spec.rb"} {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(""), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	prev, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	files, err := DiscoverFiles("spec/**/*_spec.rb", "spec/skip/**")
	if err != nil {
		t.Fatalf("DiscoverFiles error = %v", err)
	}
	if !slices.Equal(files, []string{"spec/a_spec.rb"}) {
		t.Fatalf("DiscoverFiles = %v, want [spec/a_spec.rb]", files)
	}

	cases, err := ParseJUnitXML([]byte(`<testsuites><testsuite name="s"><testcase classname="c" name="n"><failure>boom</failure></testcase></testsuite></testsuites>`))
	if err != nil {
		t.Fatalf("ParseJUnitXML error = %v", err)
	}
	if len(cases) != 1 || cases[0].Failure == nil || cases[0].Failure.Content != "boom" {
		t.Fatalf("unexpected JUnit cases: %+v", cases)
	}
}
