//go:build !windows && integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/buildkite/bktest/v3/internal/plan"
)

type capturedPlanRequest struct {
	Runner string `json:"runner"`
	Tests  struct {
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
		Selectors []struct {
			Value string `json:"value"`
		} `json:"selectors"`
	} `json:"tests"`
}

func (c capturedPlanRequest) filePaths() []string {
	out := make([]string, 0, len(c.Tests.Files)+len(c.Tests.Selectors))
	for _, f := range c.Tests.Files {
		out = append(out, f.Path)
	}
	for _, s := range c.Tests.Selectors {
		out = append(out, s.Value)
	}
	slices.Sort(out)
	return out
}

func startMockTestEngineServer(t *testing.T, selectTests func(discovered []string) []plan.TestCase) (*httptest.Server, func() capturedPlanRequest) {
	t.Helper()
	var mu sync.Mutex
	var captured capturedPlanRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/test_plan"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/test_plan/filter_tests"):
			_, _ = w.Write([]byte(`{"tests":[]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/test_plan"):
			var req capturedPlanRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			captured = req
			mu.Unlock()

			tests := selectTests(req.filePaths())
			resp := plan.TestPlan{
				Experiment: "plugin-e2e",
				Tasks: map[string]*plan.Task{
					"0": {
						NodeNumber: 0,
						Tests:      tests,
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/test_plan_metadata"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.Error(w, fmt.Sprintf("unexpected request %s %s", r.Method, r.URL.Path), http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	return srv, func() capturedPlanRequest {
		mu.Lock()
		defer mu.Unlock()
		return captured
	}
}

// TestAllRunnerPluginsE2E runs the compiled bktest CLI end-to-end against real
// test suites for all 9 extracted runners in BOTH Go and Bash plugin variants,
// verifying subdirectory loading, discovery parity, and live test execution.
func TestAllRunnerPluginsE2E(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repoRoot := filepath.Clean(filepath.Join(wd, "..", ".."))
	pluginsDir := filepath.Join(repoRoot, "plugins")
	testdataRoot := filepath.Join(repoRoot, "internal", "runner", "testdata")

	// Build real bktest CLI binary once.
	tmpBinDir := t.TempDir()
	bktestBin := filepath.Join(tmpBinDir, "bktest")
	buildCLI := exec.Command("go", "build", "-o", bktestBin, "./cmd/bktest")
	buildCLI.Dir = repoRoot
	if out, err := buildCLI.CombinedOutput(); err != nil {
		t.Fatalf("failed to build bktest CLI: %v\n%s", err, out)
	}

	// Build all 9 Go plugins into their subdirectories via bin/build-plugins.
	buildPlugins := exec.Command(filepath.Join(repoRoot, "bin", "build-plugins"))
	buildPlugins.Dir = repoRoot
	if out, err := buildPlugins.CombinedOutput(); err != nil {
		t.Fatalf("failed to build Go plugins: %v\n%s", err, out)
	}

	type runnerSuite struct {
		name           string
		workDir        string
		extraArgs      func(resultFile string) []string
		wantDiscovered string // at least this target must be discovered
		selectTarget   plan.TestCase
		wantRunner     string
	}

	suites := []runnerSuite{
		{
			name:    "rspec",
			workDir: filepath.Join(testdataRoot, "rspec"),
			extraArgs: func(res string) []string {
				return []string{
					"--test-command", "rspec --format progress --format json --out {{resultPath}} {{testExamples}}",
					"--result-path", res,
				}
			},
			wantDiscovered: "spec/spells/expelliarmus_spec.rb",
			selectTarget:   plan.TestCase{Format: plan.TestCaseFormatFile, Path: "spec/spells/expelliarmus_spec.rb"},
			wantRunner:     "rspec",
		},
		{
			name:    "jest",
			workDir: filepath.Join(testdataRoot, "jest"),
			extraArgs: func(res string) []string {
				return []string{
					"--test-command", "npx jest --json --outputFile {{resultPath}} {{testExamples}}",
					"--result-path", res,
				}
			},
			wantDiscovered: "spells/expelliarmus.spec.js",
			selectTarget:   plan.TestCase{Format: plan.TestCaseFormatFile, Path: "spells/expelliarmus.spec.js"},
			wantRunner:     "jest",
		},
		{
			name:    "vitest",
			workDir: filepath.Join(testdataRoot, "vitest"),
			extraArgs: func(res string) []string {
				return []string{
					"--test-command", "npx vitest run --reporter=json --outputFile {{resultPath}} {{testExamples}}",
					"--result-path", res,
				}
			},
			wantDiscovered: "spells/expelliarmus.spec.js",
			selectTarget:   plan.TestCase{Format: plan.TestCaseFormatFile, Path: "spells/expelliarmus.spec.js"},
			wantRunner:     "vitest",
		},
		{
			name:    "cypress",
			workDir: filepath.Join(testdataRoot, "cypress"),
			extraArgs: func(_ string) []string {
				return []string{
					"--test-command", "npx cypress run --spec {{testExamples}}",
				}
			},
			wantDiscovered: "cypress/e2e/passing_spec.cy.js",
			selectTarget:   plan.TestCase{Format: plan.TestCaseFormatFile, Path: "cypress/e2e/passing_spec.cy.js"},
			wantRunner:     "cypress",
		},
		{
			name:    "playwright",
			workDir: filepath.Join(testdataRoot, "playwright"),
			extraArgs: func(_ string) []string {
				return []string{
					"--test-command", "npx playwright test {{testExamples}}",
					"--result-path", "test-results/results.json",
				}
			},
			wantDiscovered: "tests/example.spec.js",
			selectTarget:   plan.TestCase{Format: plan.TestCaseFormatFile, Path: "tests/example.spec.js"},
			wantRunner:     "playwright",
		},
		{
			name:    "pytest",
			workDir: filepath.Join(testdataRoot, "pytest"),
			extraArgs: func(res string) []string {
				return []string{
					"--test-command", "pytest {{testExamples}} --json={{resultPath}}",
					"--result-path", res,
				}
			},
			wantDiscovered: "spells/test_expelliarmus.py",
			selectTarget:   plan.TestCase{Format: plan.TestCaseFormatFile, Path: "spells/test_expelliarmus.py"},
			wantRunner:     "pytest",
		},
		{
			name:    "gotest",
			workDir: filepath.Join(testdataRoot, "go"),
			extraArgs: func(_ string) []string {
				return []string{
					"--test-command", "go test -json {{packages}}",
				}
			},
			wantDiscovered: "example.com/hello",
			selectTarget:   plan.TestCase{Format: plan.TestCaseFormatFile, Path: "example.com/hello"},
			wantRunner:     "gotest",
		},
		{
			name:    "cucumber",
			workDir: filepath.Join(testdataRoot, "cucumber"),
			extraArgs: func(res string) []string {
				return []string{
					"--test-command", "cucumber --format progress --format json --out {{resultPath}} {{testExamples}}",
					"--result-path", res,
				}
			},
			wantDiscovered: "features/spells/expelliarmus.feature",
			selectTarget:   plan.TestCase{Format: plan.TestCaseFormatFile, Path: "features/spells/expelliarmus.feature"},
			wantRunner:     "cucumber",
		},
		{
			name:    "custom",
			workDir: filepath.Join(testdataRoot, "custom"),
			extraArgs: func(res string) []string {
				return []string{
					"--test-command", "bats --formatter junit --output " + filepath.Dir(res) + " {{testExamples}}",
					"--test-file-pattern", "tests/*_test.bats",
					"--result-path", filepath.Join(filepath.Dir(res), "report.xml"),
				}
			},
			wantDiscovered: "tests/happy_test.bats",
			selectTarget:   plan.TestCase{Format: plan.TestCaseFormatFile, Path: "tests/happy_test.bats"},
			wantRunner:     "custom",
		},
	}

	// Cycle through the three subdirectory loading modes across runners so
	// every loading mechanism is proven end-to-end through the real CLI.
	loadArgsFor := func(index int, variant, runnerName string) []string {
		switch index % 3 {
		case 0:
			// Direct subdirectory path
			return []string{"--plugin", filepath.Join(pluginsDir, variant, runnerName)}
		case 1:
			// Terraform-style root//subdir path
			return []string{"--plugin", pluginsDir + "//" + variant + "/" + runnerName}
		default:
			// Recursive --plugin-dir + --plugin-variant + --test-runner
			return []string{"--plugin-dir", pluginsDir, "--plugin-variant", variant, "--test-runner", runnerName}
		}
	}

	for idx, suite := range suites {
		suite := suite
		idx := idx
		t.Run(suite.name, func(t *testing.T) {
			discoveredByVariant := map[string][]string{}
			runnerByVariant := map[string]string{}

			for _, variant := range []string{"go", "bash"} {
				t.Run(variant, func(t *testing.T) {
					srv, getCaptured := startMockTestEngineServer(t, func(_ []string) []plan.TestCase {
						return []plan.TestCase{suite.selectTarget}
					})

					resultDir := t.TempDir()
					resultFile := filepath.Join(resultDir, suite.name+"-"+variant+".json")

					args := []string{
						"run",
						"--suite-slug", "demo-suite",
						"--access-token", "test-token",
						"--no-oidc",
						"--upload-results=false",
						"--parallelism", "1",
						"--plan-identifier", fmt.Sprintf("e2e-%s-%s", suite.name, variant),
					}
					args = append(args, loadArgsFor(idx, variant, suite.name)...)
					args = append(args, suite.extraArgs(resultFile)...)

					cmd := exec.Command(bktestBin, args...)
					cmd.Dir = suite.workDir
					var out bytes.Buffer
					cmd.Stdout = &out
					cmd.Stderr = &out

					// Ensure local environment doesn't leak Buildkite CI or analytics vars.
					env := make([]string, 0, len(os.Environ()))
					for _, kv := range os.Environ() {
						if strings.HasPrefix(kv, "BUILDKITE_") {
							continue
						}
						env = append(env, kv)
					}
					env = append(env,
						"CI=true",
						"BUILDKITE_ORGANIZATION_SLUG=demo-org",
						"BUILDKITE_PARALLEL_JOB=0",
						"BUILDKITE_TEST_ENGINE_BASE_URL="+srv.URL,
						"BUILDKITE_TEST_ENGINE_UPLOAD_RESULTS=false",
					)
					cmd.Env = env

					if err := cmd.Run(); err != nil {
						t.Fatalf("bktest run (%s/%s) failed: %v\nOutput:\n%s", variant, suite.name, err, out.String())
					}

					capReq := getCaptured()
					files := capReq.filePaths()
					if !slices.Contains(files, suite.wantDiscovered) {
						t.Fatalf("expected discovered targets to contain %q, got %v", suite.wantDiscovered, files)
					}
					if capReq.Runner != suite.wantRunner {
						t.Fatalf("reported runner = %q, want %q", capReq.Runner, suite.wantRunner)
					}
					discoveredByVariant[variant] = files
					runnerByVariant[variant] = capReq.Runner
				})
			}

			if len(discoveredByVariant) == 2 {
				if !slices.Equal(discoveredByVariant["go"], discoveredByVariant["bash"]) {
					t.Errorf("discovery mismatch between go (%v) and bash (%v)", discoveredByVariant["go"], discoveredByVariant["bash"])
				}
				if runnerByVariant["go"] != runnerByVariant["bash"] {
					t.Errorf("runner ID mismatch between go (%q) and bash (%q)", runnerByVariant["go"], runnerByVariant["bash"])
				}
			}
		})
	}
}
