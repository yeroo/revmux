package executor_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/revmux/app/executor"
	"github.com/umputun/revmux/app/executor/mocks"
	"github.com/umputun/revmux/app/finding"
)

func kimiFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // a fixture name this file passes itself
	require.NoError(t, err)
	require.NotEmpty(t, data)
	return data
}

// kimiStallCapture is the recorded version banner alone: what a kimi process has written when it is
// stuck inside its first model step, which is silent on every channel until it ends.
func kimiStallCapture(t *testing.T) []byte {
	t.Helper()
	return kimiFixture(t, "kimi-quota.jsonl")
}

func TestKimi_args(t *testing.T) {
	path := writeFixture(t, kimiFixture(t, "kimi-clean.jsonl"))
	schema := json.RawMessage(`{"type":"object","required":["findings"]}`)

	tests := []struct {
		name string
		req  executor.Request
		want []string
	}{
		{name: "model from the request", req: executor.Request{Prompt: "review this", Model: "kimi-code/kimi-for-coding", Schema: schema},
			want: []string{"-p", "review this" + executor.KimiOutputContract(schema), "--output-format", "stream-json",
				"-m", "kimi-code/kimi-for-coding"}},
		{name: "no model leaves kimi's own default", req: executor.Request{Prompt: "review this"},
			want: []string{"-p", "review this", "--output-format", "stream-json"}},
		{name: "an effort adds no flag", req: executor.Request{Prompt: "review this", Effort: "high"},
			want: []string{"-p", "review this", "--output-format", "stream-json"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := fakeRunner("emit", path)
			k := executor.NewKimi(runner, executor.Opts{KimiBin: "kimi"})
			_, err := k.Run(context.Background(), tt.req, discardSink())
			require.NoError(t, err)

			require.Len(t, runner.CommandCalls(), 1)
			call := runner.CommandCalls()[0]
			assert.Equal(t, "kimi", call.Name)
			assert.Equal(t, tt.want, call.Args)
			for _, forbidden := range []string{"--auto", "--yolo", "-y", "--plan"} {
				assert.NotContains(t, call.Args, forbidden, "kimi refuses --prompt beside any permission mode flag")
			}
		})
	}
}

func TestKimi_Run_effortIsReportedNotPassed(t *testing.T) {
	path := writeFixture(t, kimiFixture(t, "kimi-clean.jsonl"))

	t.Run("an effort is named once as info", func(t *testing.T) {
		sink := discardSink()
		k := executor.NewKimi(fakeRunner("emit", path), executor.Opts{KimiBin: "kimi"})
		_, err := k.Run(context.Background(), executor.Request{Prompt: "x", Effort: "max"}, sink)
		require.NoError(t, err)

		var notes []string
		for _, text := range eventTexts(sink, executor.EventInfo) {
			if strings.Contains(text, "effort") {
				notes = append(notes, text)
			}
		}
		require.Len(t, notes, 1)
		assert.Contains(t, notes[0], "max")
		assert.Contains(t, notes[0], "config.toml", "the note says where the effort is actually set")
	})

	t.Run("no effort says nothing about it", func(t *testing.T) {
		sink := discardSink()
		k := executor.NewKimi(fakeRunner("emit", path), executor.Opts{KimiBin: "kimi"})
		_, err := k.Run(context.Background(), executor.Request{Prompt: "x"}, sink)
		require.NoError(t, err)
		for _, text := range eventTexts(sink, executor.EventInfo) {
			assert.NotContains(t, text, "effort")
		}
	})
}

func TestKimi_Run_promptNeverReachesStdin(t *testing.T) {
	// the echo helper copies stdin to stdout, so anything written there comes back as the raw stream
	k := executor.NewKimi(fakeRunner("echo", "-"), executor.Opts{KimiBin: "kimi"})
	res, err := k.Run(context.Background(), executor.Request{Prompt: "the whole prompt"}, discardSink())
	require.NoError(t, err)
	assert.Empty(t, res.Raw, "the prompt travels in argv alone")
}

func TestKimi_Run_clean(t *testing.T) {
	data := kimiFixture(t, "kimi-clean.jsonl")
	path := writeFixture(t, data)
	raw := &bytes.Buffer{}
	sink := discardSink()

	k := executor.NewKimi(fakeRunner("emit", path), executor.Opts{KimiBin: "kimi"})
	req := executor.Request{Prompt: "x", Model: "kimi-code/kimi-for-coding", Schema: finding.FinderSchema(), RawOutput: raw}
	res, err := k.Run(context.Background(), req, sink)
	require.NoError(t, err)

	assert.Equal(t, 0, res.ExitCode)
	assert.Equal(t, data, raw.Bytes(), "archived bytes are what the process produced")
	assert.Equal(t, "kimi-code/kimi-for-coding", res.RequestedModel)
	assert.Empty(t, res.ActualModel, "kimi reports no model, and none is invented")
	assert.Zero(t, res.Tokens, "kimi reports no usage, and none is estimated")
	assert.False(t, res.RateLimited)

	var out struct {
		Findings []struct {
			File string `json:"file"`
		} `json:"findings"`
	}
	require.NoError(t, json.Unmarshal(res.StructuredOutput, &out))
	assert.NotEmpty(t, out.Findings, "the answer wins over the hook and background lines written after it")
	assert.NotEmpty(t, out.Findings[0].File)

	activity := eventTexts(sink, executor.EventActivity)
	assert.Contains(t, activity, "Reading the scoped file first to see what the change touches.")
	assert.Contains(t, activity, "Stop hook: formatted 0 files.", "a later assistant line is still activity")

	progress := eventTexts(sink, executor.EventProgress)
	assert.Equal(t, []string{"ReadFile revmux-capture/sample.go", "Shell git diff --stat"}, progress,
		"a tool call is named with what it acts on, the same short form claude's gets")

	info := eventTexts(sink, executor.EventInfo)
	require.Len(t, info, 1, "the retry is the only info line; the banner and resume hint emit nothing")
	assert.Equal(t, "retrying step, attempt 2 of 3: 503 upstream overloaded", info[0])
}

func TestKimi_Run_metaLinesDoNotOpenTheGate(t *testing.T) {
	// the version banner prints before any model call, so an output-shaped event there would release the
	// stagger gate on a process that has done nothing
	path := writeFixture(t, kimiStallCapture(t))
	sink := discardSink()
	k := executor.NewKimi(fakeRunner("emit", path), executor.Opts{KimiBin: "kimi"})
	_, err := k.Run(context.Background(), executor.Request{Prompt: "x"}, sink)
	require.NoError(t, err)
	for _, kind := range eventKinds(sink) {
		assert.NotEqual(t, executor.EventActivity, kind)
		assert.NotEqual(t, executor.EventProgress, kind)
	}
}

func TestKimi_Run_quotaFailure(t *testing.T) {
	path := writeFixture(t, kimiFixture(t, "kimi-quota.jsonl"))
	errPath := writeFixture(t, kimiFixture(t, "kimi-quota.err.txt"))
	sink := discardSink()

	k := executor.NewKimi(fakeRunner("fail", path, errPath), executor.Opts{KimiBin: "kimi"})
	res, err := k.Run(context.Background(), executor.Request{Prompt: "x", Schema: finding.FinderSchema()}, sink)

	require.NoError(t, err, "a limit is reported on the result so the pipeline decides")
	assert.Equal(t, 3, res.ExitCode)
	assert.True(t, res.RateLimited)
	assert.Equal(t, "usage limit", res.RateLimit.Status, "a failure message built from the status must say something")
	assert.Equal(t, []string{"usage limit"}, eventTexts(sink, executor.EventRateLimit))
	assert.Empty(t, res.StructuredOutput)
}

func TestKimi_Run_patternTiers(t *testing.T) {
	tests := []struct {
		name        string
		stderr      string
		wantErr     string
		wantLimited bool
	}{
		{name: "a server hiccup is a retryable failure",
			stderr: "error: failed to run prompt: provider.api_error: 503 upstream unavailable\n", wantErr: "transient failure: 503"},
		{name: "a 429 is a limit", stderr: "error: failed to run prompt: provider.rate_limit: 429 slow down\n", wantLimited: true},
		{name: "any other diagnostic is an error",
			stderr: "error: failed to run prompt: provider.auth_error: 401 invalid key\n", wantErr: "kimi failed: error: failed"},
		{name: "a limit discussed outside a diagnostic line is not one",
			stderr: "tool output mentions a rate limit and a 503\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeFixture(t, kimiStallCapture(t))
			errPath := writeFixture(t, []byte(tt.stderr))
			k := executor.NewKimi(fakeRunner("fail", path, errPath), executor.Opts{KimiBin: "kimi"})
			res, err := k.Run(context.Background(), executor.Request{Prompt: "x"}, discardSink())

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantLimited, res.RateLimited)
			assert.Equal(t, 3, res.ExitCode)
		})
	}
}

func TestKimi_Run_idleTimeout(t *testing.T) {
	// kimi's watchdog runs on its own timeout: one step is silent on every channel kimi writes, so a
	// long final answer would be killed at the shared one while the agent is working
	path := writeFixture(t, kimiStallCapture(t))

	fired := make(chan func(), 1)
	var armed []time.Duration
	var mu sync.Mutex
	clk := &mocks.ClockMock{
		NowFunc: func() time.Time { return time.Unix(0, 0).UTC() },
		AfterFuncFunc: func(d time.Duration, f func()) executor.Timer {
			mu.Lock()
			armed = append(armed, d)
			mu.Unlock()
			fired <- f
			return &mocks.TimerMock{
				StopFunc:  func() bool { return true },
				ResetFunc: func(time.Duration) bool { return true },
			}
		},
	}
	go func() { (<-fired)() }()

	opts := executor.Opts{KimiBin: "kimi", IdleTimeout: 2 * time.Minute, KimiIdleTimeout: 6 * time.Minute, Clock: clk}
	k := executor.NewKimi(fakeRunner("stall", path), opts)
	res, err := k.Run(context.Background(), executor.Request{Prompt: "x"}, discardSink())

	require.NoError(t, err, "an idle timeout is a retryable outcome, not a failure")
	assert.True(t, res.IdleTimedOut)
	assert.Empty(t, res.StructuredOutput)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []time.Duration{6 * time.Minute}, armed, "armed from KimiIdleTimeout, never the shared 2m")
}

func TestKimi_Run_startFailureNamesTheBinary(t *testing.T) {
	runner := &mocks.CommandRunnerMock{
		CommandFunc: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "revmux-no-such-binary")
		},
	}
	k := executor.NewKimi(runner, executor.Opts{KimiBin: "kimi"})
	_, err := k.Run(context.Background(), executor.Request{Prompt: "x"}, discardSink())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kimi")
}
