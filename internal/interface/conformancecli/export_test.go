package conformancecli

import "os"

// FailFreePort makes the port allocation fail until the returned function is called.
func FailFreePort(err error) (restore func()) {
	old := freePort
	freePort = func() (int, error) { return 0, err }
	return func() { freePort = old }
}

// FailMkdirAll makes making directories fail until the returned function is called.
func FailMkdirAll(err error) (restore func()) {
	old := mkdirAll
	mkdirAll = func(string, os.FileMode) error { return err }
	return func() { mkdirAll = old }
}
