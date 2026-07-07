//go:build darwin

package cli

import (
	"reflect"
	"testing"
)

func TestControllerDarwinListenerLsofAvoidsFilesystemMetadata(t *testing.T) {
	want := []string{
		"-O", "-b", "-w", "-nP", "-a",
		"-iTCP:5901", "-sTCP:LISTEN", "-Fpn",
	}
	if got := controllerDarwinListenerLsofArgs("5901"); !reflect.DeepEqual(got, want) {
		t.Fatalf("lsof args=%q want=%q", got, want)
	}
}
