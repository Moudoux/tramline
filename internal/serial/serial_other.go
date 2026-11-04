//go:build !linux

package serial

import (
	"fmt"
	"os"
	"runtime"
)

// Open always fails here. Only the Pi talks to the printer, so other systems build the
// client, and the server with --sim, which doesn't use a real port.
func Open(device string, baud int) (*os.File, error) {
	return nil, fmt.Errorf("can't open %s: real serial ports only work on Linux, not %s", device, runtime.GOOS)
}
