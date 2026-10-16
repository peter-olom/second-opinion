//go:build unix

package main

import "syscall"

func syscallSocketpair() ([2]int, error) {
	return syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
}
