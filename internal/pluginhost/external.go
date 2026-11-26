package pluginhost

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/buildkite/bktest/v3/pkg/runnerplugin"
)

// ManifestFile is the file that makes a directory a runner plugin.
const ManifestFile = "plugin.json"

// Manifest describes an external plugin without running it.
type Manifest struct {
	// Runner is the runner name bktest sends to Test Engine, such as
	// "rspec". A plugin that replaces a built-in runner keeps its name so
	// existing suites, plans and uploads keep matching.
	Runner          string `json:"runner"`
	ProtocolVersion int    `json:"protocol_version"`
	// Command runs the plugin; bktest appends the operation. A first element
	// starting with "./" is relative to the plugin directory.
	Command []string `json:"command"`
	// Version is the plugin's own version, shown in bktest's output.
	Version string `json:"version,omitempty"`

	// Dir is the plugin directory.
	Dir string `json:"-"`
}

// LoadManifest reads the manifest of the plugin at path, which is a plugin
// directory or its plugin.json.
func LoadManifest(path string) (Manifest, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("runner plugin %q: %w", path, err)
	}
	file := path
	if info.IsDir() {
		file = filepath.Join(path, ManifestFile)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return Manifest{}, fmt.Errorf("runner plugin %q: %w", path, err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("runner plugin %q: invalid %s: %w", path, filepath.Base(file), err)
	}
	if m.Runner == "" {
		return Manifest{}, fmt.Errorf("runner plugin %q: %s has no runner name", path, filepath.Base(file))
	}
	if len(m.Command) == 0 {
		return Manifest{}, fmt.Errorf("runner plugin %q: %s has no command", path, filepath.Base(file))
	}
	if m.ProtocolVersion != runnerplugin.ProtocolVersion {
		return Manifest{}, fmt.Errorf("runner plugin %q uses protocol version %d; this bktest supports version %d", path, m.ProtocolVersion, runnerplugin.ProtocolVersion)
	}
	if m.Dir, err = filepath.Abs(filepath.Dir(file)); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// String describes the plugin for bktest's output.
func (m Manifest) String() string {
	if m.Version != "" {
		return fmt.Sprintf("%s (version %s)", m.Dir, m.Version)
	}
	return m.Dir
}

// External calls an external plugin: a new process for each operation, with
// the request on stdin and the response on stdout. The process runs in
// bktest's working directory with its environment, and its stderr goes
// straight to bktest's stderr.
type External struct {
	Manifest Manifest
}

var _ Plugin = External{}

func (p External) call(operation string, request, response any) error {
	name := p.Manifest.Command[0]
	if strings.HasPrefix(name, "./") {
		name = filepath.Join(p.Manifest.Dir, name)
	}
	input, err := json.Marshal(request)
	if err != nil {
		return err
	}
	args := append(append([]string{}, p.Manifest.Command[1:]...), operation)
	// #nosec G204 -- the plugin is the one the bktest user chose with --plugin.
	cmd := exec.Command(name, args...)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stderr = os.Stderr
	output, err := cmd.Output()

	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("runner plugin %q: %s not found; build or install the plugin first", p.Manifest.Dir, name)
		}
		// A failed operation exits non-zero and says why on stdout.
		var failure runnerplugin.ErrorResponse
		if json.Unmarshal(output, &failure) == nil && failure.Error != "" {
			return errors.New(failure.Error)
		}
		return fmt.Errorf("runner plugin %q failed to %s: %w", p.Manifest.Runner, operation, err)
	}
	var version struct {
		ProtocolVersion int `json:"protocol_version"`
	}
	if err := json.Unmarshal(output, &version); err != nil {
		return fmt.Errorf("runner plugin %q sent an invalid %s response: %w: %q", p.Manifest.Runner, operation, err, truncate(output))
	}
	if version.ProtocolVersion != runnerplugin.ProtocolVersion {
		return fmt.Errorf("runner plugin %q answered %s with protocol version %d; this bktest supports version %d", p.Manifest.Runner, operation, version.ProtocolVersion, runnerplugin.ProtocolVersion)
	}
	if err := json.Unmarshal(output, response); err != nil {
		return fmt.Errorf("runner plugin %q sent an invalid %s response: %w", p.Manifest.Runner, operation, err)
	}
	return nil
}

func truncate(b []byte) string {
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}

func (p External) Configure(request runnerplugin.ConfigureRequest) (response runnerplugin.ConfigureResponse, err error) {
	err = p.call(runnerplugin.OpConfigure, request, &response)
	return response, err
}

func (p External) PrepareAttempt(request runnerplugin.PrepareAttemptRequest) (response runnerplugin.PrepareAttemptResponse, err error) {
	err = p.call(runnerplugin.OpPrepareAttempt, request, &response)
	return response, err
}

func (p External) DiscoverTargets(request runnerplugin.DiscoverTargetsRequest) (response runnerplugin.DiscoverTargetsResponse, err error) {
	err = p.call(runnerplugin.OpDiscoverTargets, request, &response)
	return response, err
}

func (p External) DiscoverExamples(request runnerplugin.DiscoverExamplesRequest) (response runnerplugin.DiscoverExamplesResponse, err error) {
	err = p.call(runnerplugin.OpDiscoverExamples, request, &response)
	return response, err
}

func (p External) InterpretAttempt(request runnerplugin.InterpretAttemptRequest) (response runnerplugin.InterpretAttemptResponse, err error) {
	err = p.call(runnerplugin.OpInterpretAttempt, request, &response)
	return response, err
}

func (p External) ParseBatchReport(request runnerplugin.ParseBatchReportRequest) (response runnerplugin.ParseBatchReportResponse, err error) {
	err = p.call(runnerplugin.OpParseBatchReport, request, &response)
	return response, err
}
