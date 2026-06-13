//go:build windows

package cli

import (
	"reflect"
	"testing"
)

func TestWindowsWebVNCDaemonTreePIDs(t *testing.T) {
	parents := map[int]int{
		100: 4,
		101: 100,
		102: 101,
		103: 100,
		200: 4,
		201: 200,
	}
	if got, want := windowsWebVNCDaemonTreePIDs(parents, 100), []int{100, 101, 102, 103}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tree=%v want %v", got, want)
	}
}

func TestTerminateMissingWindowsWebVNCDaemonTreeIsAlreadyClean(t *testing.T) {
	const impossibleTestPID = 0x7ffffffe
	if err := terminateWebVNCDaemonProcessTree(impossibleTestPID); err != nil {
		t.Fatalf("missing process tree should be clean: %v", err)
	}
}
