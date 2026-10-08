package runtime

import (
	"context"
	"errors"

	"github.com/MelloB1989/karmax/pkg/karmahelper"
	"go.uber.org/zap"
)

// All of KARMAX's model sessions — the agent's own fallback, memory
// retrieval, compaction, the loop gateway and calls — run on Claude Code when
// their provider is claude-code. This installs the path they run on.
//
// Each session kind maps to a harness kind, and the harness kind decides the
// model. A kind nobody configured falls to the harness's cheap model, never to
// whatever the CLI would pick by itself.

// sessionHarnessKinds maps a model session's usage kind to a harness kind.
var sessionHarnessKinds = map[string]string{
	"main":         "chat",
	"memory":       "memory",
	"summary":      "summary",
	"voice":        "voice",
	"loop-gateway": "gateway",
	"gateway":      "gateway",
}

func harnessKindFor(sessionKind string) string {
	if k, ok := sessionHarnessKinds[sessionKind]; ok {
		return k
	}
	return "chat"
}

// wireClaudeCodeInference points claude-code sessions at the harness.
func (rt *KarmaxRuntime) wireClaudeCodeInference() {
	if rt.harness == nil {
		// Left uninstalled on purpose: a claude-code session then fails with
		// a plain "not available", rather than quietly reaching some other
		// provider.
		rt.log.Warn("claude-code inference is unavailable: the harness is off")
		return
	}
	sup := rt.harness
	karmahelper.SetClaudeCodeRunner(func(ctx context.Context, turn karmahelper.ClaudeCodeTurn) (string, error) {
		t, err := sup.Send(ctx, turn.Key, harnessKindFor(turn.Kind), turn.Prompt)
		if err != nil {
			return "", err
		}
		if t.Text == "" {
			return "", errors.New("the harness returned an empty turn")
		}
		return t.Text, nil
	})
	rt.log.Info("model sessions run on Claude Code", zap.Int("kinds", len(sessionHarnessKinds)))
}
