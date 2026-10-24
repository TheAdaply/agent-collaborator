// Package command formats CLI examples using the invoked executable name.
package command

import (
	"os"
	"path/filepath"
	"strings"
)

func Name() string {
	if len(os.Args) == 0 || os.Args[0] == "" {
		return "ac"
	}
	return filepath.Base(os.Args[0])
}

func Text(s string) string { return strings.ReplaceAll(s, "@COMMAND@", Name()) }
