package executor

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveKimiBin(t *testing.T) {
	home := filepath.Join("home", "reviewer")
	installed := filepath.Join(home, ".kimi-code", "bin", kimiExe)
	onPath := func(string) (string, error) { return "/usr/local/bin/kimi", nil }
	notOnPath := func(string) (string, error) { return "", errors.New("not found") }
	homeDir := func() (string, error) { return home, nil }
	noHome := func() (string, error) { return "", errors.New("no home") }
	installedOnly := func(p string) bool { return p == installed }
	nothing := func(string) bool { return false }

	tests := []struct {
		name     string
		override string
		lookPath func(string) (string, error)
		home     func() (string, error)
		exists   func(string) bool
		want     string
	}{
		{name: "an override wins over everything", override: "/opt/kimi/kimi", lookPath: onPath, home: homeDir,
			exists: installedOnly, want: "/opt/kimi/kimi"},
		{name: "PATH comes before the install dir", lookPath: onPath, home: homeDir, exists: installedOnly, want: "kimi"},
		{name: "the install dir when PATH has none", lookPath: notOnPath, home: homeDir, exists: installedOnly, want: installed},
		{name: "the bare name when nothing is found, so start fails loudly", lookPath: notOnPath, home: homeDir,
			exists: nothing, want: "kimi"},
		{name: "no home directory is not a failure", lookPath: notOnPath, home: noHome, exists: installedOnly, want: "kimi"},
		{name: "a blank override is no override", override: "  ", lookPath: notOnPath, home: homeDir,
			exists: installedOnly, want: installed},
		{name: "the bare name is the default and still searches", override: "kimi", lookPath: notOnPath,
			home: homeDir, exists: installedOnly, want: installed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, resolveKimiBin(tt.override, tt.lookPath, tt.home, tt.exists))
		})
	}
}

func TestKimiAnswer(t *testing.T) {
	finder := []string{"findings"}
	answer := `{"findings":[{"title":"x"}]}`

	tests := []struct {
		name     string
		contents []string
		required []string
		want     string
	}{
		{name: "the last message when it is the answer", contents: []string{"reading files", answer},
			required: finder, want: answer},
		{name: "prose around the answer is tolerated", contents: []string{"Here it is:\n" + answer + "\nDone."},
			required: finder, want: answer},
		{name: "a later object lacking the required key does not displace the answer",
			contents: []string{answer, `Background task finished: {"task_id":"bash-1"}`, "Stop hook: ok"},
			required: finder, want: answer},
		{name: "an earlier status line holding a brace is never the answer",
			contents: []string{"checking {os.Open} handling", answer}, required: finder, want: answer},
		{name: "messages are never concatenated", contents: []string{`{"findings":`, `[]}`}, required: finder},
		{name: "no message carries the key: the last decodable one, for the pipeline to reject",
			contents: []string{`{"a":1}`, `{"b":2}`, "prose"}, required: finder, want: `{"b":2}`},
		{name: "every required key must be present", contents: []string{`{"findings":[],"open_questions":[],"pre_existing":[]}`,
			`{"findings":[]}`}, required: []string{"findings", "open_questions", "pre_existing"},
			want: `{"findings":[],"open_questions":[],"pre_existing":[]}`},
		{name: "nothing decodes", contents: []string{"no json here"}, required: finder},
		{name: "no messages", required: finder},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := kimiAnswer(tt.contents, tt.required)
			if tt.want == "" {
				assert.Empty(t, got)
				return
			}
			assert.JSONEq(t, tt.want, string(got))
		})
	}
}

func TestSchemaRequired(t *testing.T) {
	assert.Equal(t, []string{"verdicts"}, schemaRequired(json.RawMessage(`{"type":"object","required":["verdicts"]}`)))
	assert.Empty(t, schemaRequired(nil))
	assert.Empty(t, schemaRequired(json.RawMessage(`not json`)))
}

func TestEscapeWindowsArg(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", `""`},
		{"plain", "plain"},
		{"two words", `"two words"`},
		{`say "hi"`, `"say \"hi\""`},
		{`C:\dir\`, `C:\dir\`},
		{`C:\my dir\`, `"C:\my dir\\"`},
		{`a\"b`, `a\\\"b`},
		{"line one\nline two", "\"line one\nline two\""},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, escapeWindowsArg(tt.in), "input %q", tt.in)
	}
}

func TestCheckWindowsCmdLine(t *testing.T) {
	t.Run("a finder-sized prompt passes", func(t *testing.T) {
		require.NoError(t, checkWindowsCmdLine([]string{"kimi", "-p", strings.Repeat("review ", 3000)}))
	})

	t.Run("an over-long prompt names both lengths", func(t *testing.T) {
		err := checkWindowsCmdLine([]string{"kimi", "-p", strings.Repeat("x", windowsCmdLineMax)})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "32774 characters", "kimi, -p, the prompt and two separators: 4+2+32766+2")
		assert.Contains(t, err.Error(), "32766")
	})

	t.Run("exactly at the limit passes", func(t *testing.T) {
		require.NoError(t, checkWindowsCmdLine([]string{"kimi", strings.Repeat("x", windowsCmdLineMax-5)}))
	})

	t.Run("length is counted in UTF-16 units, not bytes", func(t *testing.T) {
		// U+1F600 is four bytes of UTF-8 and two UTF-16 units: 16383 of them fit, measured in bytes none would
		require.NoError(t, checkWindowsCmdLine([]string{strings.Repeat("\U0001F600", windowsCmdLineMax/2)}))
		require.Error(t, checkWindowsCmdLine([]string{strings.Repeat("\U0001F600", windowsCmdLineMax/2+1)}))
	})
}
