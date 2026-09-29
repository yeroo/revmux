# executor fixtures

Recorded from the real CLIs unless listed here, per `.claude/rules/testing.md`.

- `kimi-quota.jsonl` / `kimi-quota.err.txt` — recorded from kimi 2.1.1
  (`kimi -p '…' --output-format stream-json`, exit 1, quota exhausted), with the home path in the log
  line scrubbed to `C:\Users\reviewer`.
- `kimi-clean.jsonl` — **hand-built**, the one exception. No successful kimi run could be recorded
  while the account was over its usage limit, so each line follows the shape `PromptJsonWriter` in
  kimi 2.1.1's print mode writes, and the answer is the finder answer recorded in `codex-clean.txt`.
  The last two assistant lines stand in for a hook result and a background turn, both of which print
  mode writes after the answer. Replace it with a live capture once one can be made.
