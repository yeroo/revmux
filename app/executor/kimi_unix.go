//go:build !windows

package executor

// kimiExe is what kimi's installer writes to ~/.kimi-code/bin.
const kimiExe = "kimi"

// checkCmdLine is a no-op off Windows. Linux caps one argument at 128 KiB, four times the Windows
// line, and its failure there is "argument list too long", which already names the cause.
func (k *Kimi) checkCmdLine([]string) error { return nil }
