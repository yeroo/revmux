package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
)

// kimiName is the binary kimi installs, and the name searched for on PATH.
const kimiName = "kimi"

// windowsCmdLineMax is the longest command line CreateProcess accepts, in UTF-16 units, less the
// terminating NUL. Kimi takes its prompt in argv only, so a long prompt reaches this wall on Windows and
// would otherwise fail as an opaque "filename or extension is too long".
const windowsCmdLineMax = 32766

// kimi's stderr diagnostic is the one place its failures are worded, and matching only that line keeps a
// review that discusses a 503 from being read as one. The tiers mirror codex's: transient first, then
// limits. 500 is absent for codex's reason — it can be deterministic.
var (
	kimiRetryPattern = regexp.MustCompile(`\b(502|503|504|529)\b`)
	kimiLimitPattern = regexp.MustCompile(`(?i)usage limit|rate limit|\b429\b|quota`)
)

// Kimi runs Kimi Code CLI in print mode. Its stream-json is OpenAI-message-shaped: one assistant line
// per model step, flushed when the step ends, and no result object — the answer is prose the model
// wrote, so the output contract rides on the prompt exactly as it does for codex.
type Kimi struct {
	proc
}

// NewKimi builds a kimi executor. Its watchdog runs on KimiIdleTimeout rather than IdleTimeout: one kimi
// step is silent on stdout, stderr and its own session log alike until it ends, and a step writing a
// long answer outlasts the shared timeout while the agent is working.
func NewKimi(runner CommandRunner, opts Opts) *Kimi {
	opts.IdleTimeout = opts.KimiIdleTimeout
	bin := resolveKimiBin(opts.KimiBin, exec.LookPath, os.UserHomeDir, fileExists)
	return &Kimi{proc: newProc(bin, runner, opts)}
}

// resolveKimiBin picks the binary: an explicit override, then kimi on PATH, then the directory kimi's
// installer writes to, which it does not add to PATH on every platform. The bare name is the knob's
// default rather than an override, so it searches too. Nothing found falls back to the bare name, so a
// missing binary fails at start with the normal error rather than a revmux-invented one.
func resolveKimiBin(override string, lookPath func(string) (string, error), home func() (string, error),
	exists func(string) bool) string {
	if override = strings.TrimSpace(override); override != "" && override != kimiName {
		return override
	}
	if _, err := lookPath(kimiName); err == nil {
		return kimiName
	}
	if h, err := home(); err == nil && h != "" {
		if p := filepath.Join(h, ".kimi-code", "bin", kimiExe); exists(p) {
			return p
		}
	}
	return kimiName
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// Run executes one request. The prompt travels in argv, since print mode has no stdin input, so stdin is
// left empty. An effort cannot be passed at all: kimi reads it from its own config.toml, and the request
// saying otherwise is reported rather than silently dropped.
func (k *Kimi) Run(ctx context.Context, req Request, sink EventSink) (Result, error) {
	req.Prompt += KimiOutputContract(req.Schema)
	argv := k.args(req)
	if err := k.checkCmdLine(append([]string{k.bin}, argv...)); err != nil {
		return Result{}, err
	}
	if req.Effort != "" {
		k.emit(sink, Event{Kind: EventInfo, Text: "effort " + req.Effort +
			" ignored: kimi has no flag for it, set it in ~/.kimi-code/config.toml"})
	}

	errs := &kimiStderr{}
	required := schemaRequired(req.Schema)
	spec := runSpec{
		argv:         argv,
		sink:         sink,
		parse:        func(ctx context.Context, r io.Reader) Result { return k.parseStream(ctx, r, sink, required) },
		stderrLine:   errs.line,
		promptInArgv: true,
	}
	res, err := k.run(ctx, req, spec)
	res.RequestedModel = req.Model
	if err != nil {
		return res, err
	}
	return k.classify(res, errs.diag, sink)
}

// args builds the invocation. --auto, --yolo and --plan are never passed: print mode sets its own
// permission mode and the CLI refuses --prompt beside any of them.
func (k *Kimi) args(req Request) []string {
	argv := []string{"-p", req.Prompt, "--output-format", "stream-json"}
	if req.Model != "" {
		argv = append(argv, "-m", req.Model)
	}
	return argv
}

// KimiOutputContract is kimi's substitute for claude's --json-schema. It is codex's contract word for
// word — both CLIs answer in prose and are asked the same way — and exported for the same reason: Run
// appends it after the caller archived the composed prompt.
func KimiOutputContract(schema json.RawMessage) string {
	return CodexOutputContract(schema)
}

// kimiLine is one line of kimi's stream-json. Only the fields revmux acts on are decoded.
type kimiLine struct {
	Role      string `json:"role"`
	Type      string `json:"type"`
	Content   string `json:"content"`
	ToolCalls []struct {
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
	FailedAttempt int    `json:"failed_attempt"`
	MaxAttempts   int    `json:"max_attempts"`
	ErrorMessage  string `json:"error_message"`
}

// parseStream reports each step as it lands and picks the answer once the stream ends. Every assistant
// message is kept because the last one is not reliably the answer: a hook result and a background turn
// after the main one are both written as assistant lines too.
func (k *Kimi) parseStream(ctx context.Context, r io.Reader, sink EventSink, required []string) Result {
	var contents []string
	_ = k.readLines(ctx, r, func(line string) {
		line = strings.TrimSpace(line)
		if line == "" {
			return
		}
		var ev kimiLine
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return
		}
		switch {
		case ev.Role == "assistant":
			if ev.Content != "" {
				contents = append(contents, ev.Content)
				if text := flattenLines(ev.Content); text != "" {
					k.emit(sink, Event{Kind: EventActivity, Text: clampRunes(text)})
				}
			}
			if note := ev.progress(); note != "" {
				k.emit(sink, Event{Kind: EventProgress, Text: note})
			}
		case ev.Role == "meta" && ev.Type == "turn.step.retrying":
			k.emit(sink, Event{Kind: EventInfo, Text: ev.retrying()})
		}
		// the version banner and the resume hint emit nothing: the banner prints before any model call,
		// and anything output-shaped there would release the stagger gate early
	})
	return Result{StructuredOutput: kimiAnswer(contents, required)}
}

// progress names the step's first tool call and what it acts on, the same short form claude's gets.
func (e kimiLine) progress() string {
	for _, tc := range e.ToolCalls {
		if tc.Function.Name == "" {
			continue
		}
		b := contentBlock{Name: tc.Function.Name, Input: json.RawMessage(tc.Function.Arguments)}
		if arg := b.arg(); arg != "" {
			return b.Name + " " + arg
		}
		return b.Name
	}
	return ""
}

func (e kimiLine) retrying() string {
	text := "retrying step, attempt " + strconv.Itoa(e.FailedAttempt+1)
	if e.MaxAttempts > 0 {
		text += " of " + strconv.Itoa(e.MaxAttempts)
	}
	if msg := flattenLines(e.ErrorMessage); msg != "" {
		text += ": " + msg
	}
	return clampRunes(text)
}

// kimiAnswer walks the assistant messages last to first and takes the first object carrying every key
// the schema requires. Every object in a message is a candidate, not only the first: prose mentioning
// `struct{}{}` ahead of the answer decodes as `{}`, and stopping there degrades a source that answered.
// Each message is tried on its own, never concatenated: an earlier status line holding a brace would
// otherwise be read as the answer. When none carries them, the latest object that decodes at all is
// returned, so the pipeline's own shape check names the mismatch rather than this reporting nothing.
func kimiAnswer(contents, required []string) json.RawMessage {
	var fallback json.RawMessage
	for _, content := range slices.Backward(contents) {
		objs := jsonObjects(content)
		for _, obj := range objs {
			if carriesKeys(obj, required) {
				return obj
			}
		}
		if fallback == nil && len(objs) > 0 {
			fallback = objs[len(objs)-1]
		}
	}
	return fallback
}

// jsonObjects is every top-level object in prose, in order. A decoded object is skipped past whole, so
// the objects nested inside an answer are never candidates of their own, and an incomplete tail ends the
// search for extractJSON's reason: a nested object in a truncated answer would pass for the answer.
func jsonObjects(raw string) []json.RawMessage {
	var out []json.RawMessage
	for i := 0; i < len(raw); i++ {
		if raw[i] != '{' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(raw[i:]))
		var obj json.RawMessage
		err := dec.Decode(&obj)
		if err == nil {
			out = append(out, obj)
			i += int(dec.InputOffset()) - 1
			continue
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
	}
	return out
}

// schemaRequired reads the top-level required keys off a stage schema, so the executor tells an answer
// from a status line without knowing which stage it runs.
func schemaRequired(schema json.RawMessage) []string {
	var s struct {
		Required []string `json:"required"`
	}
	if len(schema) == 0 || json.Unmarshal(schema, &s) != nil {
		return nil
	}
	return s.Required
}

func carriesKeys(raw json.RawMessage, keys []string) bool {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false
	}
	for _, key := range keys {
		if _, ok := obj[key]; !ok {
			return false
		}
	}
	return true
}

// classify tiers a failed run from kimi's stderr diagnostic alone. stdout is structured, so unlike codex
// there is no prose tail to consult, and the diagnostic is where the provider's status code surfaces.
// A five-hour usage limit lands in the limit tier, and the one retry the pipeline makes cannot succeed
// there — it is still a limit, and reporting it as anything else would misdescribe it.
func (k *Kimi) classify(res Result, diag string, sink EventSink) (Result, error) {
	if res.ExitCode == 0 || diag == "" {
		return res, nil
	}
	if m := kimiRetryPattern.FindString(diag); m != "" && !res.IdleTimedOut {
		return res, fmt.Errorf("kimi transient failure: %s", m)
	}
	if m := kimiLimitPattern.FindString(diag); m != "" {
		p := strings.ToLower(m)
		res.RateLimited = true
		res.RateLimit = RateLimitInfo{Status: p, RateLimitType: p}
		k.emit(sink, Event{Kind: EventRateLimit, Text: p})
		return res, nil
	}
	if !res.IdleTimedOut {
		return res, fmt.Errorf("kimi failed: %s", diag)
	}
	return res, nil
}

// kimiStderr keeps the last CLI diagnostic. Per-run state, so never a field on Kimi. The rest of stderr
// is tool progress text, which is liveness only and reaches the watchdog without passing through here.
type kimiStderr struct {
	diag string
}

func (s *kimiStderr) line(l string) {
	if t := strings.TrimSpace(l); strings.HasPrefix(strings.ToLower(t), "error:") {
		s.diag = t
	}
}

// checkWindowsCmdLine refuses a command line CreateProcess would reject, naming both lengths, before
// anything starts. It measures the line the way os/exec builds it — each argument escaped by the
// CommandLineToArgvW rules and joined by spaces — and counts UTF-16 units, since that is what the limit
// is in.
func checkWindowsCmdLine(args []string) error {
	n := 0
	for i, a := range args {
		if i > 0 {
			n++
		}
		for _, r := range escapeWindowsArg(a) {
			n += utf16.RuneLen(r)
		}
	}
	if n > windowsCmdLineMax {
		return fmt.Errorf("kimi command line is %d characters, over the Windows limit of %d: "+
			"the prompt travels in argv and is too long for one kimi process", n, windowsCmdLineMax)
	}
	return nil
}

// escapeWindowsArg is syscall.EscapeArg, restated because that function exists on Windows alone and the
// measurement is tested on every platform, while only Windows enforces it. A Windows-only test pins the two together.
func escapeWindowsArg(s string) string {
	if s == "" {
		return `""`
	}
	needsBackslash := strings.ContainsAny(s, `"\`)
	hasSpace := strings.ContainsAny(s, " \t")
	if !needsBackslash && !hasSpace {
		return s
	}
	if !needsBackslash {
		return `"` + s + `"`
	}

	var b strings.Builder
	if hasSpace {
		b.WriteByte('"')
	}
	slashes := 0
	for i := range len(s) {
		c := s[i]
		switch c {
		case '\\':
			slashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, slashes+1))
			slashes = 0
		default:
			slashes = 0
		}
		b.WriteByte(c)
	}
	if hasSpace {
		b.WriteString(strings.Repeat(`\`, slashes))
		b.WriteByte('"')
	}
	return b.String()
}
