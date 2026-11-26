// Package plugin handles discovering, compiling/caching, loading, and invoking
// external bktest runner plugins from files, directories, and subdirectories.
package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"

	proto "github.com/buildkite/bktest/v3/pkg/plugin"
)

const ManifestFilename = "plugin.json"

// ResolveOptions configures how bktest locates a runner plugin.
type ResolveOptions struct {
	// PluginPath is an explicit path to a plugin directory (which may be a
	// subdirectory, including Terraform-style "root//subdir" syntax) or to a
	// standalone executable.
	PluginPath string
	// TestRunner is the runner name (e.g. "rspec") or a direct path to a plugin.
	TestRunner string
	// PluginDirs are additional directories (searched recursively across
	// subdirectories) containing plugin.json manifests.
	PluginDirs []string
	// PluginVariant optionally restricts discovery to a plugin kind ("go" or "bash").
	PluginVariant string
}

// DiscoveredPlugin represents a plugin manifest found on disk before its
// executable is resolved or built.
type DiscoveredPlugin struct {
	Dir        string
	Executable string
	Manifest   proto.Manifest
}

// LoadedPlugin is a ready-to-invoke external plugin executable.
type LoadedPlugin struct {
	Dir        string
	Executable string
	Manifest   proto.Manifest
}

var (
	buildMu    sync.Mutex
	builtCache = map[string]string{}
)

// ResolveSubdirPath resolves a plugin reference that may use either a normal
// filesystem path ("./plugins/bash/rspec") or Terraform-style double-slash
// subdirectory syntax ("./plugins//bash/rspec").
func ResolveSubdirPath(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if root, sub, ok := strings.Cut(ref, "//"); ok {
		return filepath.Clean(filepath.Join(root, filepath.FromSlash(sub)))
	}
	return filepath.Clean(ref)
}

// LooksLikePath reports whether a runner selector is a filesystem path rather
// than a logical runner name.
func LooksLikePath(value string) bool {
	if value == "" {
		return false
	}
	if strings.Contains(value, "//") || strings.Contains(value, "/") || strings.Contains(value, `\`) || strings.HasPrefix(value, ".") {
		return true
	}
	if _, err := os.Stat(value); err == nil {
		return true
	}
	return false
}

// Resolve locates and loads a plugin according to opts.
func Resolve(opts ResolveOptions) (*LoadedPlugin, error) {
	if opts.PluginPath != "" {
		return Load(opts.PluginPath)
	}

	if LooksLikePath(opts.TestRunner) {
		return Load(opts.TestRunner)
	}

	if opts.TestRunner == "" {
		return nil, fmt.Errorf("test runner or plugin path must be specified")
	}

	dirs := effectiveSearchDirs(opts.PluginDirs)
	discovered, err := DiscoverInDirs(dirs)
	if err != nil {
		return nil, err
	}

	var matches []DiscoveredPlugin
	for _, p := range discovered {
		if p.Manifest.Name != opts.TestRunner {
			continue
		}
		if opts.PluginVariant != "" && p.Manifest.Kind != opts.PluginVariant {
			continue
		}
		matches = append(matches, p)
	}

	if len(matches) > 0 {
		chosen := chooseMatch(matches, opts.PluginVariant)
		return materialize(chosen)
	}

	// Fallback: look for bktest-plugin-<name> on PATH when no variant filter
	// explicitly excludes external binaries.
	if opts.PluginVariant == "" || opts.PluginVariant == "go" || opts.PluginVariant == "bash" {
		binName := "bktest-plugin-" + opts.TestRunner
		if binPath, err := exec.LookPath(binName); err == nil {
			return Load(binPath)
		}
	}

	available := availableRunnerNames(discovered)
	if opts.PluginVariant != "" {
		return nil, fmt.Errorf("no %q plugin found for runner %q (available runners: %s)", opts.PluginVariant, opts.TestRunner, strings.Join(available, ", "))
	}
	if len(available) > 0 {
		return nil, fmt.Errorf("runner value %q is invalid, available plugins: %s", opts.TestRunner, strings.Join(available, ", "))
	}
	return nil, fmt.Errorf("runner value %q is invalid, no plugins found in search directories %v", opts.TestRunner, dirs)
}

// ResolveByReportFormat locates a plugin that declares support for parsing a
// native batch report format (such as "rspec-json" or "playwright-json").
func ResolveByReportFormat(format string, dirs []string, variant string) (*LoadedPlugin, error) {
	if format == "" {
		return nil, fmt.Errorf("unsupported runner report format %q", format)
	}
	searchDirs := effectiveSearchDirs(dirs)
	discovered, err := DiscoverInDirs(searchDirs)
	if err != nil {
		return nil, err
	}

	var matches []DiscoveredPlugin
	for _, p := range discovered {
		if !slices.Contains(p.Manifest.ReportFormats, format) {
			continue
		}
		if variant != "" && p.Manifest.Kind != variant {
			continue
		}
		matches = append(matches, p)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("unsupported runner report format %q", format)
	}
	return materialize(chooseMatch(matches, variant))
}

// Load loads a plugin from a directory containing plugin.json (including nested
// subdirectories or "root//subdir" references) or from a standalone executable.
func Load(ref string) (*LoadedPlugin, error) {
	resolved := ResolveSubdirPath(ref)
	absPath, err := filepath.Abs(resolved)
	if err != nil {
		return nil, fmt.Errorf("resolve plugin path %q: %w", ref, err)
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("load plugin %q: %w", ref, err)
	}

	if !info.IsDir() {
		name := strings.TrimPrefix(filepath.Base(absPath), "bktest-plugin-")
		name = strings.TrimSuffix(name, filepath.Ext(name))
		return &LoadedPlugin{
			Dir:        filepath.Dir(absPath),
			Executable: absPath,
			Manifest: proto.Manifest{
				ProtocolVersion: proto.ProtocolVersion,
				Name:            name,
				Entrypoint:      absPath,
			},
		}, nil
	}

	manifest, err := loadManifest(absPath)
	if err != nil {
		return nil, err
	}
	return materialize(DiscoveredPlugin{
		Dir:      absPath,
		Manifest: manifest,
	})
}

// DiscoverInDirs recursively scans dirs and their subdirectories for plugins
// defined by plugin.json manifests.
func DiscoverInDirs(dirs []string) ([]DiscoveredPlugin, error) {
	var found []DiscoveredPlugin
	seenDirs := map[string]bool{}

	for _, rawDir := range dirs {
		if rawDir == "" {
			continue
		}
		dir, err := filepath.Abs(ResolveSubdirPath(rawDir))
		if err != nil {
			continue
		}
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}

		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if d.IsDir() {
				base := d.Name()
				if path != dir && (base == ".git" || base == "node_modules" || base == "vendor" || base == "testdata" || base == "dist" || base == ".amp" || base == "tmp") {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Name() != ManifestFilename {
				return nil
			}
			pluginDir := filepath.Dir(path)
			if seenDirs[pluginDir] {
				return nil
			}
			seenDirs[pluginDir] = true

			manifest, err := loadManifest(pluginDir)
			if err != nil {
				return nil
			}
			found = append(found, DiscoveredPlugin{
				Dir:      pluginDir,
				Manifest: manifest,
			})
			return nil
		})
	}

	sort.Slice(found, func(i, j int) bool {
		if found[i].Manifest.Name != found[j].Manifest.Name {
			return found[i].Manifest.Name < found[j].Manifest.Name
		}
		if found[i].Manifest.Kind != found[j].Manifest.Kind {
			return found[i].Manifest.Kind < found[j].Manifest.Kind
		}
		return found[i].Dir < found[j].Dir
	})
	return found, nil
}

func loadManifest(dir string) (proto.Manifest, error) {
	manifestPath := filepath.Join(dir, ManifestFilename)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return proto.Manifest{}, fmt.Errorf("read plugin manifest %s: %w", manifestPath, err)
	}
	var manifest proto.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return proto.Manifest{}, fmt.Errorf("parse plugin manifest %s: %w", manifestPath, err)
	}
	if manifest.ProtocolVersion != proto.ProtocolVersion {
		return proto.Manifest{}, fmt.Errorf("plugin manifest %s uses unsupported protocol_version %d (want %d)", manifestPath, manifest.ProtocolVersion, proto.ProtocolVersion)
	}
	if manifest.Entrypoint == "" {
		return proto.Manifest{}, fmt.Errorf("plugin manifest %s is missing entrypoint", manifestPath)
	}
	return manifest, nil
}

func materialize(dp DiscoveredPlugin) (*LoadedPlugin, error) {
	entry := dp.Manifest.Entrypoint
	if !filepath.IsAbs(entry) {
		entry = filepath.Join(dp.Dir, filepath.FromSlash(entry))
	}
	if runtime.GOOS == "windows" && filepath.Ext(entry) == "" {
		if _, err := os.Stat(entry + ".exe"); err == nil {
			entry += ".exe"
		}
	}

	if info, err := os.Stat(entry); err == nil && !info.IsDir() {
		return &LoadedPlugin{
			Dir:        dp.Dir,
			Executable: entry,
			Manifest:   dp.Manifest,
		}, nil
	}

	if dp.Manifest.Build != nil && dp.Manifest.Build.GoPackage != "" {
		binPath, err := ensureGoPluginBuilt(dp.Dir, dp.Manifest)
		if err != nil {
			return nil, err
		}
		return &LoadedPlugin{
			Dir:        dp.Dir,
			Executable: binPath,
			Manifest:   dp.Manifest,
		}, nil
	}

	return nil, fmt.Errorf("plugin %q entrypoint %q does not exist", dp.Manifest.Name, entry)
}

func ensureGoPluginBuilt(pluginDir string, manifest proto.Manifest) (string, error) {
	buildMu.Lock()
	defer buildMu.Unlock()

	fingerprint, err := goPluginFingerprint(pluginDir)
	if err != nil {
		return "", fmt.Errorf("fingerprint Go plugin %q: %w", manifest.Name, err)
	}

	cacheKey := pluginDir + "@" + fingerprint
	if cached, ok := builtCache[cacheKey]; ok {
		if _, err := os.Stat(cached); err == nil {
			return cached, nil
		}
	}

	cacheBase, err := os.UserCacheDir()
	if err != nil || cacheBase == "" {
		cacheBase = os.TempDir()
	}
	cacheDir := filepath.Join(cacheBase, "bktest", "plugins")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		cacheDir = filepath.Join(os.TempDir(), "bktest-plugins")
		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			return "", fmt.Errorf("create plugin build cache dir: %w", err)
		}
	}

	binName := fmt.Sprintf("bktest-plugin-%s-%s", sanitizeName(manifest.Name), fingerprint[:12])
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	target := filepath.Join(cacheDir, binName)
	if _, err := os.Stat(target); err == nil {
		builtCache[cacheKey] = target
		return target, nil
	}

	// Build into a unique temp file in cacheDir, then atomically rename.
	tmpFile, err := os.CreateTemp(cacheDir, binName+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("create temp plugin binary: %w", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

	cmd := exec.Command("go", "build", "-o", tmpPath, manifest.Build.GoPackage)
	cmd.Dir = pluginDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("build Go plugin %q in %s: %w\n%s", manifest.Name, pluginDir, err, string(out))
	}

	if err := os.Rename(tmpPath, target); err != nil {
		// Another concurrent build may have won the race; if target exists, use it.
		if _, statErr := os.Stat(target); statErr != nil {
			return "", fmt.Errorf("install compiled Go plugin %q: %w", manifest.Name, err)
		}
	}

	builtCache[cacheKey] = target
	return target, nil
}

func goPluginFingerprint(pluginDir string) (string, error) {
	h := sha256.New()
	_, _ = io.WriteString(h, pluginDir+"\n")
	_, _ = io.WriteString(h, runtime.GOOS+"/"+runtime.GOARCH+"\n")

	// Include plugin directory Go files and pkg/plugin SDK files when inside repo.
	dirsToHash := []string{pluginDir}
	if repoRoot := findRepoRootFromSource(); repoRoot != "" {
		sdkDir := filepath.Join(repoRoot, "pkg", "plugin")
		if info, err := os.Stat(sdkDir); err == nil && info.IsDir() {
			dirsToHash = append(dirsToHash, sdkDir)
		}
	}

	for _, dir := range dirsToHash {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return "", err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if !strings.HasSuffix(name, ".go") && name != ManifestFilename {
				continue
			}
			path := filepath.Join(dir, name)
			data, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			_, _ = io.WriteString(h, path+"\x00")
			_, _ = h.Write(data)
			_, _ = io.WriteString(h, "\x00")
		}
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "plugin"
	}
	return b.String()
}

func chooseMatch(matches []DiscoveredPlugin, preferredVariant string) DiscoveredPlugin {
	if preferredVariant != "" {
		for _, m := range matches {
			if m.Manifest.Kind == preferredVariant {
				return m
			}
		}
	}
	// Default preference: compiled Go plugin first, then Bash, then first match.
	for _, kind := range []string{"go", "bash"} {
		for _, m := range matches {
			if m.Manifest.Kind == kind {
				return m
			}
		}
	}
	return matches[0]
}

func availableRunnerNames(discovered []DiscoveredPlugin) []string {
	seen := map[string]bool{}
	var names []string
	for _, d := range discovered {
		if d.Manifest.Name != "" && !seen[d.Manifest.Name] {
			seen[d.Manifest.Name] = true
			names = append(names, d.Manifest.Name)
		}
	}
	sort.Strings(names)
	return names
}

func effectiveSearchDirs(explicit []string) []string {
	var dirs []string
	add := func(d string) {
		if d == "" {
			return
		}
		abs, err := filepath.Abs(ResolveSubdirPath(d))
		if err != nil {
			return
		}
		if !slices.Contains(dirs, abs) {
			dirs = append(dirs, abs)
		}
	}

	for _, entry := range explicit {
		for _, part := range filepath.SplitList(entry) {
			add(part)
		}
	}

	if envDirs := os.Getenv("BUILDKITE_TEST_ENGINE_PLUGIN_DIR"); envDirs != "" {
		for _, part := range filepath.SplitList(envDirs) {
			add(part)
		}
	}

	// Next to the running binary.
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		add(filepath.Join(exeDir, "plugins"))
		add(filepath.Join(exeDir, "..", "plugins"))
	}

	// Walk up from cwd looking for a plugins/ directory.
	if cwd, err := os.Getwd(); err == nil {
		cur := cwd
		for {
			candidate := filepath.Join(cur, "plugins")
			if info, err := os.Stat(candidate); err == nil && info.IsDir() {
				add(candidate)
				break
			}
			parent := filepath.Dir(cur)
			if parent == cur {
				break
			}
			cur = parent
		}
	}

	// Development / go test fallback when a test chdirs into a temp directory.
	if repoRoot := findRepoRootFromSource(); repoRoot != "" {
		candidate := filepath.Join(repoRoot, "plugins")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			add(candidate)
		}
	}

	return dirs
}

func findRepoRootFromSource() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	// internal/plugin/loader.go -> repo root is two levels up from internal/plugin.
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
