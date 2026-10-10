//go:build !linux

package goprt

import "os"

var saved [3]*os.File

// mute swaps the os.Std* variables for /dev/null. Unlike the Linux version it
// cannot silence writers that captured the original files (such as log).
func mute() {
	if saved[1] != nil {
		return
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return
	}
	saved = [3]*os.File{os.Stdin, os.Stdout, os.Stderr}
	os.Stdin, os.Stdout, os.Stderr = null, null, null
}

func unmute() {
	if saved[1] == nil {
		return
	}
	os.Stdin, os.Stdout, os.Stderr = saved[0], saved[1], saved[2]
	saved = [3]*os.File{}
}
