// Package pluginhost loads runner plugins from a directory and calls them over
// the runnerplugin protocol.
package pluginhost

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/bktest/v3/pkg/runnerplugin"
)

// ManifestFile is the file that makes a directory a runner plugin.
const ManifestFile = "plugin.json"

// Manifest describes a plugin without starting it.
type Manifest struct {
	// Runner is the runner name bktest sends to Test Engine, such as
	// "rspec". A plugin that replaces a built-in runner keeps its name so
	// existing suites, plans and uploads keep matching.
	Runner          string `json:"runner"`
	ProtocolVersion int    `json:"protocol_version"`
	// Command starts the plugin. A first element starting with "./" is
	// relative to the plugin directory.
	Command []string `json:"command"`

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

// Client is a running plugin process.
type Client struct {
	manifest Manifest
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   *bufio.Reader
	nextID   int64
	mu       sync.Mutex
	closed   bool
}

// Start starts the plugin. The plugin runs in bktest's working directory
// with bktest's environment, and its stderr goes to bktest's stderr.
func Start(m Manifest) (*Client, error) {
	name := m.Command[0]
	if strings.HasPrefix(name, "./") {
		name = filepath.Join(m.Dir, name)
	}
	cmd := exec.Command(name, m.Command[1:]...)
	cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%d", runnerplugin.ProtocolEnv, runnerplugin.ProtocolVersion))
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("runner plugin %q: %s not found; build or install the plugin first", m.Dir, name)
		}
		return nil, fmt.Errorf("runner plugin %q: %w", m.Dir, err)
	}
	return &Client{manifest: m, cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}, nil
}

// Call sends one request and decodes its result into result.
func (c *Client) Call(method string, params, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("runner plugin %q is closed", c.manifest.Runner)
	}

	c.nextID++
	req := runnerplugin.Request{JSONRPC: "2.0", ID: c.nextID, Method: method}
	if params != nil {
		data, err := json.Marshal(params)
		if err != nil {
			return err
		}
		req.Params = data
	}
	line, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if _, err := c.stdin.Write(append(line, '\n')); err != nil {
		return c.exitError(method, err)
	}

	respLine, err := c.stdout.ReadBytes('\n')
	if err != nil {
		return c.exitError(method, err)
	}
	var resp runnerplugin.Response
	if err := json.Unmarshal(respLine, &resp); err != nil {
		return fmt.Errorf("runner plugin %q sent an invalid %s response: %w: %q", c.manifest.Runner, method, err, truncate(respLine))
	}
	if resp.ID != req.ID {
		return fmt.Errorf("runner plugin %q answered request %d with %d", c.manifest.Runner, req.ID, resp.ID)
	}
	if resp.Error != nil {
		return resp.Error
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(resp.Result, result); err != nil {
		return fmt.Errorf("runner plugin %q sent an invalid %s result: %w", c.manifest.Runner, method, err)
	}
	return nil
}

func (c *Client) exitError(method string, err error) error {
	return fmt.Errorf("runner plugin %q stopped during %s: %w", c.manifest.Runner, method, err)
}

// Close closes the plugin's stdin, which asks it to exit, and kills it if it
// hasn't exited after a few seconds.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	_ = c.stdin.Close()
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		_ = c.cmd.Process.Kill()
		return <-done
	}
}

func truncate(b []byte) string {
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}
