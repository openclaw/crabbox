//go:build !darwin

package lume

func systemForeignVMUse(string) (string, error) {
	return "", nil
}
