package goprt

import "syscall"

// saved holds duplicates of the real stdin/stdout/stderr while muted.
var saved = [3]int{-1, -1, -1}

// mute points fds 0-2 at /dev/null at the file-descriptor level, so output
// from fmt, log, println and child processes of replayed cells is all hidden,
// and replayed reads from stdin see EOF instead of blocking.
func mute() {
	if saved[0] >= 0 {
		return
	}
	null, err := syscall.Open("/dev/null", syscall.O_RDWR, 0)
	if err != nil {
		return
	}
	for fd := 0; fd < 3; fd++ {
		if dup, err := syscall.Dup(fd); err == nil {
			saved[fd] = dup
			syscall.Dup3(null, fd, 0)
		}
	}
	syscall.Close(null)
}

func unmute() {
	for fd := 0; fd < 3; fd++ {
		if saved[fd] >= 0 {
			syscall.Dup3(saved[fd], fd, 0)
			syscall.Close(saved[fd])
			saved[fd] = -1
		}
	}
}
