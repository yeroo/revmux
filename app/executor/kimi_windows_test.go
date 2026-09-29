//go:build windows

package executor

import (
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestEscapeWindowsArg_matchesSyscall pins the restated escaping to the one os/exec actually uses, which
// exists on Windows alone.
func TestEscapeWindowsArg_matchesSyscall(t *testing.T) {
	for _, in := range []string{"", "plain", "two words", `say "hi"`, `C:\dir\`, `C:\my dir\`, `a\"b`,
		"line one\nline two", "tab\there", `trailing\\`, `"`, `\\"quoted\\"`, "ünïcode 😀 `tick`"} {
		assert.Equal(t, syscall.EscapeArg(in), escapeWindowsArg(in), "input %q", in)
	}
}
