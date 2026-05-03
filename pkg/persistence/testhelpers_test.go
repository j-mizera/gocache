package persistence

import "os"

// writeFileFn is the implementation backing the test helper. Kept here so
// the trivial os import doesn't pollute every test file.
func writeFileFn(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}
