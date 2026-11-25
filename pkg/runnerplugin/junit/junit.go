// Package junit parses JUnit XML reports for runner plugins.
package junit

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
)

// TestCase represents a single <testcase> element in JUnit XML.
type TestCase struct {
	Classname string   `xml:"classname,attr"`
	Name      string   `xml:"name,attr"`
	Result    string   // passed | failed | skipped
	Failure   *Failure `xml:"failure"`
	Error     *Error   `xml:"error"`
	Skipped   *Skipped `xml:"skipped"`
	// SuiteName is the name attribute of the enclosing <testsuite> element.
	SuiteName string `xml:"-"`
}

// Failure represents the <failure> element in JUnit XML
type Failure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Content string `xml:",chardata"`
}

// Error represents the <error> element in JUnit XML
type Error struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Content string `xml:",chardata"`
}

// Skipped represents the <skipped> element in JUnit XML
type Skipped struct {
	Message string `xml:"message,attr"`
}

type junitXMLTestSuite struct {
	XMLName   xml.Name   `xml:"testsuite"`
	Name      string     `xml:"name,attr"`
	TestCases []TestCase `xml:"testcase"`
}

type junitXMLTestSuites struct {
	XMLName    xml.Name            `xml:"testsuites"`
	TestSuites []junitXMLTestSuite `xml:"testsuite"`
}

func Load(path string) ([]TestCase, error) {
	xmlFile, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open JUnit XML file %s: %w", path, err)
	}
	defer xmlFile.Close()

	byteValue, err := io.ReadAll(xmlFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read JUnit XML file %s: %w", path, err)
	}

	results, err := Parse(byteValue)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal JUnit XML file %s: %w", path, err)
	}
	return results, nil
}

func Parse(data []byte) ([]TestCase, error) {
	var testSuites junitXMLTestSuites
	if err := xml.Unmarshal(data, &testSuites); err != nil {
		return nil, err
	}

	var results []TestCase
	for _, suite := range testSuites.TestSuites {
		for _, tc := range suite.TestCases {
			testCase := tc
			testCase.SuiteName = suite.Name
			if testCase.Failure != nil || testCase.Error != nil {
				testCase.Result = "failed"
			} else if testCase.Skipped != nil {
				testCase.Result = "skipped"
			} else {
				testCase.Result = "passed"
			}
			results = append(results, testCase)
		}
	}

	return results, nil
}
