package command

import (
	"os"
	"testing"
)

func TestInvokedName(t *testing.T) {
	saved := os.Args
	defer func() { os.Args = saved }()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"/opt/bin/agent-collaborator"}, "agent-collaborator"},
		{[]string{"/opt/bin/ac"}, "ac"},
		{nil, "ac"},
		{[]string{""}, "ac"},
	} {
		os.Args = tc.args
		if got := Text("usage: @COMMAND@ ask; @COMMAND@ help"); got != "usage: "+tc.want+" ask; "+tc.want+" help" {
			t.Errorf("args %v: %q", tc.args, got)
		}
	}
}
