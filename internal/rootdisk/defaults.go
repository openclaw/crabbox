// Package rootdisk defines the shared class policy for resizable root disks.
package rootdisk

// DefaultGB is a floor; providers must also honor the source image's minimum.
func DefaultGB(class string) int64 {
	switch class {
	case "tiny":
		return 40
	case "small":
		return 80
	case "standard", "fast":
		return 150
	case "large":
		return 250
	default:
		return 400
	}
}
