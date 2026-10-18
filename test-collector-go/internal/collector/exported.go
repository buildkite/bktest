package collector

import (
	"fmt"
	"regexp"
)

// ExportedLine is what bktest.Main prints on stderr after exporting a
// package's results. go test merges the test binary's stderr into its
// -json stream, so a buildkite-test-collector process piping that stream
// sees the line and can warn that the package is being reported twice.
func ExportedLine(count int, importPath string) string {
	return fmt.Sprintf("buildkite-test-collector: exported %d test executions from %s", count, importPath)
}

var exportedLine = regexp.MustCompile(`^buildkite-test-collector: exported \d+ test executions from (\S+)\n?$`)

// ExportedPackage returns the import path named by an ExportedLine, or
// false when line is anything else.
func ExportedPackage(line string) (string, bool) {
	m := exportedLine.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	return m[1], true
}
