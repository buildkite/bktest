package runnerplugin

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
)

// JUnitTestCase is a test case in a JUnit XML report.
type JUnitTestCase struct {
	Classname string        `xml:"classname,attr"`
	Name      string        `xml:"name,attr"`
	Result    string        // StatusPassed, StatusFailed or StatusSkipped
	Failure   *JUnitFailure `xml:"failure"`
	Error     *JUnitError   `xml:"error"`
	Skipped   *JUnitSkipped `xml:"skipped"`
	// SuiteName is the name of the test suite the test case is in.
	SuiteName string `xml:"-"`
}

type JUnitFailure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Content string `xml:",chardata"`
}

type JUnitError struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Content string `xml:",chardata"`
}

type JUnitSkipped struct {
	Message string `xml:"message,attr"`
}

type junitTestSuite struct {
	XMLName   xml.Name        `xml:"testsuite"`
	Name      string          `xml:"name,attr"`
	TestCases []JUnitTestCase `xml:"testcase"`
}

type junitTestSuites struct {
	XMLName    xml.Name         `xml:"testsuites"`
	TestSuites []junitTestSuite `xml:"testsuite"`
}

// ReadJUnitXML reads the test cases of the JUnit XML report at path.
func ReadJUnitXML(path string) ([]JUnitTestCase, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open JUnit XML file %s: %w", path, err)
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("failed to read JUnit XML file %s: %w", path, err)
	}
	results, err := ParseJUnitXML(data)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal JUnit XML file %s: %w", path, err)
	}
	return results, nil
}

// ParseJUnitXML returns the test cases of a JUnit XML report whose root
// element is <testsuites>. A test case with a failure or an error failed.
func ParseJUnitXML(data []byte) ([]JUnitTestCase, error) {
	var testSuites junitTestSuites
	if err := xml.Unmarshal(data, &testSuites); err != nil {
		return nil, err
	}

	var results []JUnitTestCase
	for _, suite := range testSuites.TestSuites {
		for _, testCase := range suite.TestCases {
			testCase.SuiteName = suite.Name
			if testCase.Failure != nil || testCase.Error != nil {
				testCase.Result = StatusFailed
			} else if testCase.Skipped != nil {
				testCase.Result = StatusSkipped
			} else {
				testCase.Result = StatusPassed
			}
			results = append(results, testCase)
		}
	}
	return results, nil
}
