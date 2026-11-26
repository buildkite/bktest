package plugin

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	proto "github.com/buildkite/bktest/v3/pkg/plugin"
)

func repoPluginsDir(t *testing.T) string {
	t.Helper()
	root := findRepoRootFromSource()
	if root == "" {
		t.Fatal("could not locate repository root")
	}
	return filepath.Join(root, "plugins")
}

func TestResolveSubdirPath(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"./plugins/go/rspec", "plugins/go/rspec"},
		{"/abs/plugins//bash/pytest", "/abs/plugins/bash/pytest"},
		{"./plugins//go/vitest", "plugins/go/vitest"},
		{"  plugins//bash/cypress  ", "plugins/bash/cypress"},
	}
	for _, tc := range tests {
		if got := ResolveSubdirPath(tc.in); got != tc.want {
			t.Errorf("ResolveSubdirPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLooksLikePath(t *testing.T) {
	for _, pathLike := range []string{"./plugins/go/rspec", "../p", "/tmp/p", "plugins//bash/jest", "dir/sub"} {
		if !LooksLikePath(pathLike) {
			t.Errorf("LooksLikePath(%q) = false, want true", pathLike)
		}
	}
	for _, nameOnly := range []string{"rspec", "pytest", "gotest", "custom"} {
		if LooksLikePath(nameOnly) {
			t.Errorf("LooksLikePath(%q) = true, want false", nameOnly)
		}
	}
}

func TestDiscoverInDirsFindsAllGoAndBashPlugins(t *testing.T) {
	pluginsDir := repoPluginsDir(t)
	discovered, err := DiscoverInDirs([]string{pluginsDir})
	if err != nil {
		t.Fatalf("DiscoverInDirs() error = %v", err)
	}

	wantRunners := []string{"cucumber", "custom", "cypress", "gotest", "jest", "playwright", "pytest", "rspec", "vitest"}
	gotNames := availableRunnerNames(discovered)
	if !slices.Equal(gotNames, wantRunners) {
		t.Fatalf("discovered runners = %v, want %v", gotNames, wantRunners)
	}

	byKey := map[string]DiscoveredPlugin{}
	for _, dp := range discovered {
		key := dp.Manifest.Kind + "/" + dp.Manifest.Name
		byKey[key] = dp
	}
	for _, kind := range []string{"go", "bash"} {
		for _, runner := range wantRunners {
			key := kind + "/" + runner
			dp, ok := byKey[key]
			if !ok {
				t.Errorf("missing %s plugin", key)
				continue
			}
			if dp.Manifest.ProtocolVersion != proto.ProtocolVersion {
				t.Errorf("%s protocol_version = %d, want %d", key, dp.Manifest.ProtocolVersion, proto.ProtocolVersion)
			}
		}
	}
}

func TestResolveSubdirectoryFormats(t *testing.T) {
	pluginsDir := repoPluginsDir(t)

	cases := []struct {
		name        string
		opts        ResolveOptions
		wantName    string
		wantKind    string
		wantDisplay string
	}{
		{
			name:        "direct subdirectory go",
			opts:        ResolveOptions{PluginPath: filepath.Join(pluginsDir, "go", "rspec")},
			wantName:    "rspec",
			wantKind:    "go",
			wantDisplay: "RSpec",
		},
		{
			name:        "direct subdirectory bash",
			opts:        ResolveOptions{PluginPath: filepath.Join(pluginsDir, "bash", "rspec")},
			wantName:    "rspec",
			wantKind:    "bash",
			wantDisplay: "RSpec",
		},
		{
			name:        "terraform-style root//subdir go",
			opts:        ResolveOptions{PluginPath: pluginsDir + "//go/pytest"},
			wantName:    "pytest",
			wantKind:    "go",
			wantDisplay: "pytest",
		},
		{
			name:        "terraform-style root//subdir bash",
			opts:        ResolveOptions{PluginPath: pluginsDir + "//bash/pytest"},
			wantName:    "pytest",
			wantKind:    "bash",
			wantDisplay: "pytest",
		},
		{
			name: "runner name with plugin-dir and go variant",
			opts: ResolveOptions{
				TestRunner:    "vitest",
				PluginDirs:    []string{pluginsDir},
				PluginVariant: "go",
			},
			wantName:    "vitest",
			wantKind:    "go",
			wantDisplay: "Vitest",
		},
		{
			name: "runner name with plugin-dir and bash variant",
			opts: ResolveOptions{
				TestRunner:    "vitest",
				PluginDirs:    []string{pluginsDir},
				PluginVariant: "bash",
			},
			wantName:    "vitest",
			wantKind:    "bash",
			wantDisplay: "Vitest",
		},
		{
			name:        "runner name containing subdirectory path",
			opts:        ResolveOptions{TestRunner: filepath.Join(pluginsDir, "bash", "gotest")},
			wantName:    "gotest",
			wantKind:    "bash",
			wantDisplay: "gotest",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loaded, err := Resolve(tc.opts)
			if err != nil {
				t.Fatalf("Resolve(%+v) error = %v", tc.opts, err)
			}
			if loaded.Manifest.Name != tc.wantName || loaded.Manifest.Kind != tc.wantKind {
				t.Fatalf("got manifest %s/%s, want %s/%s", loaded.Manifest.Kind, loaded.Manifest.Name, tc.wantKind, tc.wantName)
			}

			var cfgResp proto.ConfigureResponse
			if err := loaded.Invoke(proto.OpConfigure, proto.ConfigureRequest{
				ProtocolVersion: proto.ProtocolVersion,
				Config:          proto.RunnerSettings{},
			}, &cfgResp); err != nil {
				t.Fatalf("Invoke(configure) error = %v", err)
			}
			if cfgResp.Name != tc.wantDisplay {
				t.Errorf("configure display name = %q, want %q", cfgResp.Name, tc.wantDisplay)
			}
		})
	}
}

func TestResolveByReportFormat(t *testing.T) {
	pluginsDir := repoPluginsDir(t)
	formats := map[string]string{
		"rspec-json":      "rspec",
		"jest-json":       "jest",
		"playwright-json": "playwright",
		"pytest-json":     "pytest",
		"pytest-junit":    "pytest",
	}

	for _, variant := range []string{"go", "bash"} {
		for format, wantRunner := range formats {
			t.Run(variant+"/"+format, func(t *testing.T) {
				loaded, err := ResolveByReportFormat(format, []string{pluginsDir}, variant)
				if err != nil {
					t.Fatalf("ResolveByReportFormat(%q, %q) error = %v", format, variant, err)
				}
				if loaded.Manifest.Name != wantRunner || loaded.Manifest.Kind != variant {
					t.Fatalf("got %s/%s, want %s/%s", loaded.Manifest.Kind, loaded.Manifest.Name, variant, wantRunner)
				}
			})
		}
	}
}

func TestLoadStandaloneExecutableAndPATHLookup(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "bktest-plugin-demo")
	script := `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  configure)
    printf '{"protocol_version":1,"runner_id":"demo","name":"Demo","config":{"test_file_pattern":"*_demo.txt"},"features":{"auto_retry":true},"capabilities":{"interpret_attempt":false}}\n'
    ;;
  *)
    printf '{"protocol_version":1,"error":"unsupported op %s"}\n' "${1:-}"
    exit 1
    ;;
esac
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// Direct executable path.
	loaded, err := Load(binPath)
	if err != nil {
		t.Fatalf("Load(standalone executable) error = %v", err)
	}
	var resp proto.ConfigureResponse
	if err := loaded.Invoke(proto.OpConfigure, proto.ConfigureRequest{ProtocolVersion: proto.ProtocolVersion}, &resp); err != nil {
		t.Fatalf("Invoke(configure) error = %v", err)
	}
	if resp.Name != "Demo" {
		t.Fatalf("resp.Name = %q, want Demo", resp.Name)
	}

	// PATH lookup for bktest-plugin-demo.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	fromPath, err := Resolve(ResolveOptions{
		TestRunner: "demo",
		PluginDirs: []string{t.TempDir()},
	})
	if err != nil {
		t.Fatalf("Resolve via PATH error = %v", err)
	}
	if fromPath.Executable != binPath {
		t.Fatalf("Executable = %q, want %q", fromPath.Executable, binPath)
	}

	// Structured error envelope on unknown operation.
	err = loaded.Invoke("unknown-op", map[string]any{"protocol_version": 1}, &resp)
	if err == nil || !strings.Contains(err.Error(), "unsupported op unknown-op") {
		t.Fatalf("expected structured plugin error, got: %v", err)
	}
}

func TestLoadRejectsInvalidManifestAndProtocolMismatch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(`{"protocol_version":99,"name":"bad","entrypoint":"./run.sh"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "protocol_version") {
		t.Fatalf("expected protocol_version mismatch error, got: %v", err)
	}

	emptyDir := t.TempDir()
	if _, err := Load(emptyDir); err == nil || !strings.Contains(err.Error(), "plugin.json") {
		t.Fatalf("expected missing plugin.json error, got: %v", err)
	}
}
