package serial

import (
	"bufio"
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// openPTY gives a real TTY to test against, since CI has no USB adapter.
func openPTY(t *testing.T) (*os.File, string) {
	t.Helper()
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	t.Cleanup(func() { ptmx.Close() })
	var unlock int32
	const tiocsptlck, tiocgptn = 0x40045431, 0x80045430
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, ptmx.Fd(), tiocsptlck, uintptr(unsafe.Pointer(&unlock))); e != 0 {
		t.Fatal(e)
	}
	var n uint32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, ptmx.Fd(), tiocgptn, uintptr(unsafe.Pointer(&n))); e != 0 {
		t.Fatal(e)
	}
	return ptmx, fmt.Sprintf("/dev/pts/%d", n)
}

func TestOpenRoundTrip(t *testing.T) {
	ptmx, path := openPTY(t)
	port, err := Open(path, 250000)
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()

	if _, err := ptmx.WriteString("ok T:21.59\n"); err != nil {
		t.Fatal(err)
	}
	port.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(port).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != "ok T:21.59\n" {
		t.Fatalf("read %q", line)
	}

	if _, err := port.WriteString("M115\n"); err != nil {
		t.Fatal(err)
	}
	line, err = bufio.NewReader(ptmx).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != "M115\n" {
		t.Fatalf("wrote %q", line)
	}
}

func TestOpenMissingDevice(t *testing.T) {
	if _, err := Open("/dev/does-not-exist", 250000); err == nil {
		t.Fatal("expected an error")
	}
}
