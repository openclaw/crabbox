package cli

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

type junitTestSuites struct {
	XMLName  xml.Name         `xml:"testsuites"`
	Name     string           `xml:"name,attr"`
	Tests    int              `xml:"tests,attr"`
	Failures int              `xml:"failures,attr"`
	Errors   int              `xml:"errors,attr"`
	Skipped  int              `xml:"skipped,attr"`
	Time     float64          `xml:"time,attr"`
	Suites   []junitTestSuite `xml:"testsuite"`
}

type junitTestSuite struct {
	XMLName   xml.Name        `xml:"testsuite"`
	Name      string          `xml:"name,attr"`
	Tests     int             `xml:"tests,attr"`
	Failures  int             `xml:"failures,attr"`
	Errors    int             `xml:"errors,attr"`
	Skipped   int             `xml:"skipped,attr"`
	Time      float64         `xml:"time,attr"`
	TestCases []junitTestCase `xml:"testcase"`
}

type junitTestCase struct {
	Name      string         `xml:"name,attr"`
	Classname string         `xml:"classname,attr"`
	File      string         `xml:"file,attr"`
	Time      float64        `xml:"time,attr"`
	Failures  []junitFailure `xml:"failure"`
	Errors    []junitFailure `xml:"error"`
	Skipped   []struct{}     `xml:"skipped"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Text    string `xml:",chardata"`
}

func parseJUnitResults(files map[string]string) (*TestResultSummary, error) {
	if len(files) == 0 {
		return nil, nil
	}
	summary := &TestResultSummary{
		Format: "junit",
		Files:  make([]string, 0, len(files)),
		Failed: []TestFailure{},
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var parseErrors []error
	for _, name := range names {
		data := files[name]
		trimmed := strings.TrimSpace(data)
		if trimmed == "" {
			continue
		}
		fileSummary := &TestResultSummary{Format: "junit", Failed: []TestFailure{}}
		if err := addJUnitFile(fileSummary, strings.NewReader(trimmed)); err != nil {
			parseErrors = append(parseErrors, fmt.Errorf("skip junit %s: %w", name, err))
			continue
		}
		mergeJUnitSummary(summary, fileSummary)
		summary.Files = append(summary.Files, name)
	}
	if len(summary.Files) == 0 {
		return nil, errors.Join(parseErrors...)
	}
	return summary, errors.Join(parseErrors...)
}

func mergeJUnitSummary(dst, src *TestResultSummary) {
	dst.Suites += src.Suites
	dst.Tests += src.Tests
	dst.Failures += src.Failures
	dst.Errors += src.Errors
	dst.Skipped += src.Skipped
	dst.TimeSeconds += src.TimeSeconds
	dst.Failed = append(dst.Failed, src.Failed...)
}

func addJUnitFile(summary *TestResultSummary, input io.Reader) error {
	decoder := xml.NewDecoder(input)
	for {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "testsuites":
			var suites junitTestSuites
			if err := decoder.DecodeElement(&suites, &start); err != nil {
				return err
			}
			addJUnitSuites(summary, suites)
			return nil
		case "testsuite":
			var suite junitTestSuite
			if err := decoder.DecodeElement(&suite, &start); err != nil {
				return err
			}
			addJUnitSuite(summary, suite)
			return nil
		default:
			return fmt.Errorf("unsupported root element %q", start.Name.Local)
		}
	}
}

func addJUnitSuites(summary *TestResultSummary, suites junitTestSuites) {
	if len(suites.Suites) == 0 {
		summary.Suites++
		summary.Tests += suites.Tests
		summary.Failures += suites.Failures
		summary.Errors += suites.Errors
		summary.Skipped += suites.Skipped
		summary.TimeSeconds += suites.Time
		return
	}
	for _, suite := range suites.Suites {
		addJUnitSuite(summary, suite)
	}
}

func addJUnitSuite(summary *TestResultSummary, suite junitTestSuite) {
	summary.Suites++
	derivedTests := len(suite.TestCases)
	derivedFailures := 0
	derivedErrors := 0
	derivedSkipped := 0
	derivedTime := 0.0
	for _, tc := range suite.TestCases {
		derivedTime += tc.Time
		derivedFailures += len(tc.Failures)
		derivedErrors += len(tc.Errors)
		derivedSkipped += len(tc.Skipped)
	}
	if suite.Tests > 0 || suite.Failures > 0 || suite.Errors > 0 || suite.Skipped > 0 {
		summary.Tests += suite.Tests
		if suite.Tests == 0 {
			summary.Tests += derivedTests
		}
		summary.Failures += max(suite.Failures, derivedFailures)
		summary.Errors += max(suite.Errors, derivedErrors)
		summary.Skipped += max(suite.Skipped, derivedSkipped)
		if suite.Time > 0 {
			summary.TimeSeconds += suite.Time
		} else {
			summary.TimeSeconds += derivedTime
		}
	} else {
		summary.Tests += derivedTests
		summary.Failures += derivedFailures
		summary.Errors += derivedErrors
		summary.Skipped += derivedSkipped
		summary.TimeSeconds += derivedTime
	}
	for _, tc := range suite.TestCases {
		for _, failure := range tc.Failures {
			summary.Failed = append(summary.Failed, testFailureFromJUnit(suite, tc, failure, "failure"))
		}
		for _, failure := range tc.Errors {
			summary.Failed = append(summary.Failed, testFailureFromJUnit(suite, tc, failure, "error"))
		}
	}
}

func testFailureFromJUnit(suite junitTestSuite, tc junitTestCase, failure junitFailure, kind string) TestFailure {
	message := strings.TrimSpace(failure.Message)
	if message == "" {
		message = strings.TrimSpace(failure.Text)
	}
	return TestFailure{
		Suite:     suite.Name,
		Name:      tc.Name,
		Classname: tc.Classname,
		File:      tc.File,
		Message:   message,
		Type:      failure.Type,
		Kind:      kind,
	}
}
