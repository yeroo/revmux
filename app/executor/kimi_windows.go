//go:build windows

package executor

// kimiExe is what kimi's installer writes to ~/.kimi-code/bin.
const kimiExe = "kimi.exe"

// checkCmdLine enforces the CreateProcess limit, which only Windows has: kimi takes its prompt in argv.
func (k *Kimi) checkCmdLine(args []string) error { return checkWindowsCmdLine(args) }
