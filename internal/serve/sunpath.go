package serve

import "runtime"

// sunPathMax is the kernel's limit on a Unix socket path, from sockaddr_un's
// sun_path field. Exceeding it fails with a bare "bind: invalid argument", so
// the length is checked explicitly rather than left to a mystery errno.
var sunPathMax = func() int {
	if runtime.GOOS == "linux" {
		return 108
	}
	return 104 // darwin and the BSDs
}()
