//go:build unix

package node

import "syscall"

func umask(m int) int { return syscall.Umask(m) }
