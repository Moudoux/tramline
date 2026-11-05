// Package webui holds the browser files: the read-only page the server shows, the GUI
// that tramline gui shows, and what they share, such as the 3D view.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed page shared gui
var files embed.FS

// Page is the read-only page served by the Pi.
func Page() fs.FS { return sub("page") }

// GUI is the page tramline gui serves on this machine.
func GUI() fs.FS { return sub("gui") }

// Shared is what both use, served under /shared/.
func Shared() fs.FS { return sub("shared") }

func sub(dir string) fs.FS {
	out, err := fs.Sub(files, dir)
	if err != nil {
		panic(err)
	}
	return out
}
