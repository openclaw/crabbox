//go:build !darwin

package lume

func systemForeignLumeVMDirectoryUse(string) (string, error) {
	return "", nil
}
