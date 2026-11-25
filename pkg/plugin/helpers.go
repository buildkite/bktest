package plugin

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"io/fs"
	"os"

	"drjosh.dev/zzglob"
	"github.com/kballard/go-shellquote"
)

// DiscoverFiles walks the current working directory using zzglob include and
// exclude patterns, skipping node_modules and directory entries.
func DiscoverFiles(pattern string, excludePattern string) ([]string, error) {
	parsedPattern, err := zzglob.Parse(pattern)
	if err != nil {
		return nil, fmt.Errorf("error parsing test file pattern %q", pattern)
	}

	parsedExcludePattern, err := zzglob.Parse(excludePattern)
	if err != nil {
		return nil, fmt.Errorf("error parsing test file exclude pattern %q", excludePattern)
	}

	discoveredFiles := []string{}
	err = parsedPattern.Glob(func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error walking at path %q: %v\n", path, err)
			return nil
		}

		if parsedExcludePattern.Match(path) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		if d.Name() == "node_modules" {
			return fs.SkipDir
		}

		if d.IsDir() {
			return nil
		}

		discoveredFiles = append(discoveredFiles, path)
		return nil
	}, zzglob.WalkIntermediateDirs(true))
	if err != nil {
		return nil, fmt.Errorf("error walking directory: %v", err)
	}

	if len(discoveredFiles) == 0 {
		return nil, fmt.Errorf("no files found with pattern %q and exclude pattern %q", pattern, excludePattern)
	}

	return discoveredFiles, nil
}

// PathsFromTestCases extracts runnable selector or path values from test cases.
func PathsFromTestCases(testCases []TestCase) []string {
	paths := make([]string, len(testCases))
	for i, tc := range testCases {
		if tc.Format == TestCaseFormatSelector {
			paths[i] = tc.Value
		} else {
			paths[i] = tc.Path
		}
	}
	return paths
}

// TestCasesFromPaths wraps raw paths as TestCase values.
func TestCasesFromPaths(paths []string) []TestCase {
	testCases := make([]TestCase, len(paths))
	for i, path := range paths {
		testCases[i] = TestCase{Path: path}
	}
	return testCases
}

// TestIdentifier returns the canonical scope/name/path key matching RunResult.
func TestIdentifier(tc TestCase) string {
	return tc.Scope + "/" + tc.Name + "/" + tc.Path
}

// SplitCommand splits a command string using POSIX shell quoting rules.
func SplitCommand(cmd string) ([]string, error) {
	words, err := shellquote.Split(cmd)
	if err != nil {
		return nil, err
	}
	if len(words) == 0 {
		return nil, fmt.Errorf("test command is empty")
	}
	return words, nil
}

// TestEngineTest represents a Test Engine JSON test result object.
type TestEngineTest struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Scope    string            `json:"scope"`
	Location string            `json:"location"`
	FileName string            `json:"file_name,omitempty"`
	Result   string            `json:"result"`
	Tags     map[string]string `json:"tags,omitempty"`
}

// ParseTestEngineTestResult loads and decodes a Test Engine JSON results file.
func ParseTestEngineTestResult(path string) ([]TestEngineTest, error) {
	var results []TestEngineTest
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read json: %v", err)
	}
	if err := json.Unmarshal(data, &results); err != nil {
		return nil, fmt.Errorf("failed to parse json: %v", err)
	}
	return results, nil
}

// JUnitXMLTestCase represents a single <testcase> element in JUnit XML.
type JUnitXMLTestCase struct {
	Classname string           `xml:"classname,attr"`
	Name      string           `xml:"name,attr"`
	Result    string           `xml:"-"`
	Failure   *JUnitXMLFailure `xml:"failure"`
	Error     *JUnitXMLError   `xml:"error"`
	Skipped   *JUnitXMLSkipped `xml:"skipped"`
	SuiteName string           `xml:"-"`
}

type JUnitXMLFailure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Content string `xml:",chardata"`
}

type JUnitXMLError struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Content string `xml:",chardata"`
}

type JUnitXMLSkipped struct {
	Message string `xml:"message,attr"`
}

type junitXMLTestSuite struct {
	XMLName   xml.Name           `xml:"testsuite"`
	Name      string             `xml:"name,attr"`
	TestCases []JUnitXMLTestCase `xml:"testcase"`
}

type junitXMLTestSuites struct {
	XMLName    xml.Name            `xml:"testsuites"`
	TestSuites []junitXMLTestSuite `xml:"testsuite"`
}

// LoadAndParseJUnitXML reads and parses a JUnit XML file from disk.
func LoadAndParseJUnitXML(path string) ([]JUnitXMLTestCase, error) {
	xmlFile, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open JUnit XML file %s: %w", path, err)
	}
	defer xmlFile.Close()

	byteValue, err := io.ReadAll(xmlFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read JUnit XML file %s: %w", path, err)
	}

	results, err := ParseJUnitXML(byteValue)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal JUnit XML file %s: %w", path, err)
	}
	return results, nil
}

// ParseJUnitXML decodes JUnit XML bytes into test case outcomes.
func ParseJUnitXML(data []byte) ([]JUnitXMLTestCase, error) {
	var testSuites junitXMLTestSuites
	if err := xml.Unmarshal(data, &testSuites); err != nil {
		return nil, err
	}

	var results []JUnitXMLTestCase
	for _, suite := range testSuites.TestSuites {
		for _, tc := range suite.TestCases {
			testCase := tc
			testCase.SuiteName = suite.Name
			if testCase.Failure != nil || testCase.Error != nil {
				testCase.Result = TestStatusFailed
			} else if testCase.Skipped != nil {
				testCase.Result = TestStatusSkipped
			} else {
				testCase.Result = TestStatusPassed
			}
			results = append(results, testCase)
		}
	}
	return results, nil
}
