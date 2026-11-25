package pluginhost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/bktest/v3/pkg/runnerplugin"
)

func writePlugin(t *testing.T, manifest, script string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "plugins", "fake")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if script != "" {
		if err := os.WriteFile(filepath.Join(dir, "plugin.sh"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadManifest(t *testing.T) {
	dir := writePlugin(t, `{"runner": "fake", "protocol_version": 1, "command": ["./plugin.sh"]}`, "")
	for _, path := range []string{dir, filepath.Join(dir, ManifestFile)} {
		m, err := LoadManifest(path)
		if err != nil {
			t.Fatal(err)
		}
		if m.Runner != "fake" || m.Dir != dir {
			t.Errorf("LoadManifest(%q) = %+v, want runner fake in %s", path, m, dir)
		}
	}

	dir = writePlugin(t, `{"runner": "fake", "protocol_version": 2, "command": ["./plugin.sh"]}`, "")
	if _, err := LoadManifest(dir); err == nil || !strings.Contains(err.Error(), "protocol version 2") {
		t.Errorf("LoadManifest with protocol version 2: err = %v, want a protocol version error", err)
	}
}

// A plugin's command is relative to its directory, it gets the protocol
// environment variable, and a plugin that exits mid-request fails the call.
func TestClientCall(t *testing.T) {
	dir := writePlugin(t, `{"runner": "fake", "protocol_version": 1, "command": ["./plugin.sh", "arg"]}`, `#!/bin/sh
read -r line
printf '{"jsonrpc":"2.0","id":1,"result":{"protocol_version":%s,"name":"%s"}}\n' "$BKTEST_RUNNER_PLUGIN_PROTOCOL" "$1"
read -r line
printf '{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"dry run failed"}}\n'
read -r line
exit 3
`)
	m, err := LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Start(m)
	if err != nil {
		t.Fatal(err)
	}

	var info runnerplugin.InitializeResult
	if err := c.Call(runnerplugin.MethodInitialize, runnerplugin.InitializeParams{}, &info); err != nil {
		t.Fatal(err)
	}
	if info.ProtocolVersion != 1 || info.Name != "arg" {
		t.Errorf("initialize = %+v, want protocol version 1 and name arg", info)
	}
	if err := c.Call(runnerplugin.MethodExamples, nil, nil); err == nil || err.Error() != "dry run failed" {
		t.Errorf("examples error = %v, want the plugin's message", err)
	}
	if err := c.Call(runnerplugin.MethodDiscover, nil, nil); err == nil || !strings.Contains(err.Error(), "stopped during discover") {
		t.Errorf("discover error = %v, want the plugin stopping", err)
	}
	if err := c.Close(); err == nil {
		t.Error("Close() = nil, want the plugin's exit status")
	}
}

func TestStart_MissingExecutable(t *testing.T) {
	dir := writePlugin(t, `{"runner": "fake", "protocol_version": 1, "command": ["./bktest-runner-fake"]}`, "")
	m, err := LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Start(m); err == nil || !strings.Contains(err.Error(), "build or install the plugin first") {
		t.Errorf("Start() error = %v, want a hint to build the plugin", err)
	}
}
