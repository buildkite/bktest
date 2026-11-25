package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	proto "github.com/buildkite/bktest/v3/pkg/plugin"
)

// Invoke calls a single plugin operation over stdin/stdout JSON.
func (p *LoadedPlugin) Invoke(operation string, request any, response any) error {
	payload, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode %s request for plugin %q: %w", operation, p.Manifest.Name, err)
	}

	cmd := exec.Command(p.Executable, operation)
	cmd.Stdin = bytes.NewReader(payload)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	// Check whether the plugin emitted a structured JSON error response.
	rawOut := bytes.TrimSpace(stdout.Bytes())
	if len(rawOut) > 0 {
		var errEnvelope struct {
			ProtocolVersion int    `json:"protocol_version"`
			Error           string `json:"error"`
		}
		if json.Unmarshal(rawOut, &errEnvelope) == nil && errEnvelope.Error != "" {
			return fmt.Errorf("%s", errEnvelope.Error)
		}
	}

	if runErr != nil {
		errText := strings.TrimSpace(stderr.String())
		if errText != "" {
			return fmt.Errorf("plugin %q (%s) failed: %s", p.Manifest.Name, operation, errText)
		}
		return fmt.Errorf("plugin %q (%s) failed: %w", p.Manifest.Name, operation, runErr)
	}

	// Forward non-error stderr output (such as plugin warnings or diagnostics)
	// directly to stderr so users still see runner messages.
	if stderr.Len() > 0 {
		_, _ = os.Stderr.Write(stderr.Bytes())
	}

	if len(rawOut) == 0 {
		return fmt.Errorf("plugin %q (%s) returned empty output", p.Manifest.Name, operation)
	}

	if err := json.Unmarshal(rawOut, response); err != nil {
		return fmt.Errorf("decode %s response from plugin %q: %w (stdout=%q)", operation, p.Manifest.Name, err, string(rawOut))
	}

	var versionCheck struct {
		ProtocolVersion int `json:"protocol_version"`
	}
	if err := json.Unmarshal(rawOut, &versionCheck); err == nil && versionCheck.ProtocolVersion != 0 && versionCheck.ProtocolVersion != proto.ProtocolVersion {
		return fmt.Errorf("plugin %q returned unsupported protocol_version %d (want %d)", p.Manifest.Name, versionCheck.ProtocolVersion, proto.ProtocolVersion)
	}

	return nil
}
