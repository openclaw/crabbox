//go:build !darwin || !arm64

package utm

import (
	"context"
	"fmt"
)

func (*backend) nativeMacOSPreflight(context.Context, string) (macOSImage, error) {
	return macOSImage{}, fmt.Errorf("fresh macOS requires a local Apple Silicon Mac with macOS 27+, full Xcode and UTM Automation access")
}
