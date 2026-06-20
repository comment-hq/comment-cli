//go:build unix

package main

import "syscall"

// reexecComment replaces the current process with the freshly installed binary,
// preserving the original argv and the supplied environment. On success it does
// not return (the process image is replaced).
func reexecComment(bin string, argv []string, env []string) error {
	args := make([]string, 0, len(argv))
	args = append(args, bin)
	if len(argv) > 1 {
		args = append(args, argv[1:]...)
	}
	return syscall.Exec(bin, args, env)
}
