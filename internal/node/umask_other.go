//go:build !unix

package node

func umask(m int) int { return m }
