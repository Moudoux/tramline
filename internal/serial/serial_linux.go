// Package serial opens a USB serial port at an arbitrary baud rate.
package serial

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// Values from the Linux UAPI headers, which are the same on arm64 and amd64.
const (
	tcsets2 = 0x402C542B
	bother  = 0o010000
	cs8     = 0o60
	cread   = 0o200
	clocal  = 0o4000
	vtime   = 5
	vmin    = 6
)

// termios2 is the kernel's struct, which unlike termios allows custom baud rates.
// Marlin's 250000 isn't one of the standard rates.
type termios2 struct {
	iflag, oflag, cflag, lflag uint32
	line                       uint8
	cc                         [19]uint8
	ispeed, ospeed             uint32
}

// Open returns the port in raw mode. HUPCL is left off so closing the port
// doesn't drop DTR, which could otherwise restart the board.
func Open(device string, baud int) (*os.File, error) {
	f, err := os.OpenFile(device, os.O_RDWR|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	t := termios2{
		cflag:  bother | cs8 | cread | clocal,
		ispeed: uint32(baud),
		ospeed: uint32(baud),
	}
	t.cc[vmin] = 1
	t.cc[vtime] = 0

	conn, err := f.SyscallConn()
	if err == nil {
		ctlErr := conn.Control(func(fd uintptr) {
			if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, tcsets2, uintptr(unsafe.Pointer(&t))); errno != 0 {
				err = errno
			}
		})
		if err == nil {
			err = ctlErr
		}
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("configure %s: %w", device, err)
	}
	return f, nil
}
