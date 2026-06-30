package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/scheduler"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"go.uber.org/zap"
)

// startLoopkitLoops schedules every loop registered via the public loopkit SDK
// (third-party + built-in installed loops) and runs them when they fire. They
// are registered with an empty AgentID so the bus->agent router ignores them —
// the runner below executes them directly via a loopKit, bypassing the agent
// prompt path (a loopkit loop is real Go code, not a prompt).
func (rt *KarmaxRuntime) startLoopkitLoops(ctx context.Context) {
	loops := loopkit.Registered()
	if len(loops) == 0 {
		return
	}

	yamlNames := map[string]bool{}
	for _, l := range rt.cfg.Loops {
		yamlNames[l.Name] = true
	}
	defaultAgent := ""
	if len(rt.cfg.Agents) > 0 {
		defaultAgent = rt.cfg.Agents[0].ID
	}

	byName := make(map[string]loopkit.Loop, len(loops))
	for _, l := range loops {
		if yamlNames[l.Name] {
			rt.log.Warn("loopkit loop name clashes with a yaml loop; skipping", zap.String("loop", l.Name))
			continue
		}
		byName[l.Name] = l
		if err := rt.scheduler.AddJob(scheduler.ScheduledJob{
			ID:      "loopkit:" + l.Name,
			Name:    "loopkit:" + l.Name,
			Cron:    l.Schedule.CronExpr(),
			AgentID: "", // empty => agent router skips it; our runner handles it
			Payload: map[string]any{"loopkit": l.Name},
			Enabled: true,
		}); err != nil {
			rt.log.Error("failed to register loopkit loop", zap.String("loop", l.Name), zap.Error(err))
			delete(byName, l.Name)
			continue
		}
		rt.log.Info("registered loopkit loop",
			zap.String("loop", l.Name), zap.String("schedule", l.Schedule.CronExpr()))
	}
	if len(byName) == 0 {
		return
	}

	sub, cancel := rt.bus.Subscribe(bus.EventScheduledJob)
	go func() {
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case evt, ok := <-sub.Ch:
				if !ok {
					return
				}
				inner, _ := evt.Payload["payload"].(map[string]any)
				if inner == nil {
					continue
				}
				name, _ := inner["loopkit"].(string)
				if l, found := byName[name]; found {
					go rt.runLoopkitLoop(ctx, l, defaultAgent)
				}
			}
		}
	}()
}

func (rt *KarmaxRuntime) runLoopkitLoop(parent context.Context, l loopkit.Loop, agentID string) {
	ctx, cancel := context.WithTimeout(parent, 12*time.Minute)
	defer cancel()

	ns := agentID
	if len(rt.cfg.Agents) > 0 && rt.cfg.Agents[0].Memory.Namespace != "" {
		ns = rt.cfg.Agents[0].Memory.Namespace
	}
	wacliPath := rt.cfg.ColdScan.WacliPath
	if wacliPath == "" {
		wacliPath = "/home/mellob/code/wacli/wacli"
	}

	k := &loopKit{
		loopName:  l.Name,
		agentID:   agentID,
		rt:        rt,
		mem:       rt.memory.For(agentID, ns),
		wacliPath: wacliPath,
	}
	rt.log.Info("running loopkit loop", zap.String("loop", l.Name))
	if err := l.Run(ctx, k); err != nil {
		rt.log.Warn("loopkit loop failed", zap.String("loop", l.Name), zap.Error(err))
		return
	}
	rt.log.Info("loopkit loop done", zap.String("loop", l.Name))
}

// loopKit implements loopkit.Kit, exposing KARMAX capabilities to authored loops.
type loopKit struct {
	loopName  string
	agentID   string
	rt        *KarmaxRuntime
	mem       *memory.Manager
	wacliPath string
}

func (k *loopKit) Ask(ctx context.Context, prompt string) (string, error) {
	ag, ok := k.rt.agents.Get(k.agentID)
	if !ok || ag == nil {
		return "", fmt.Errorf("agent %q unavailable", k.agentID)
	}
	return ag.Chat(ctx, prompt)
}

func (k *loopKit) Harness(ctx context.Context, prompt string) (string, error) {
	tool := &builtin.ClaudeCodeTool{Store: k.rt.store, AgentID: k.agentID}
	res, err := tool.Execute(ctx, map[string]any{"prompt": prompt})
	if err != nil {
		return "", err
	}
	if res.IsError {
		return "", fmt.Errorf("harness: %s", res.Error)
	}
	return loopToolField(res, "output"), nil
}

func (k *loopKit) Remember(fact string) error {
	if k.mem == nil {
		return fmt.Errorf("memory unavailable")
	}
	return k.mem.Write(memory.MemoryEntry{Role: "assistant", Content: fact, Tags: []string{"loop", k.loopName}})
}

func (k *loopKit) Recall(query string, limit int) ([]string, error) {
	if k.mem == nil {
		return nil, fmt.Errorf("memory unavailable")
	}
	if limit <= 0 {
		limit = 8
	}
	res, err := k.mem.SearchSemantic(query, limit)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(res))
	for _, r := range res {
		out = append(out, r.Entry.Content)
	}
	return out, nil
}

func (k *loopKit) Notify(title, body string) error {
	tool := &builtin.AppPushTool{Store: k.rt.store, AgentID: k.agentID}
	res, err := tool.Execute(context.Background(), map[string]any{"title": title, "body": body, "kind": "loop"})
	if err != nil {
		return err
	}
	if res.IsError {
		return fmt.Errorf("notify: %s", res.Error)
	}
	return nil
}

func (k *loopKit) SendWhatsApp(_ context.Context, target, content string) error {
	if k.rt.comms == nil {
		return fmt.Errorf("comms manager unavailable")
	}
	if strings.TrimSpace(target) == "" {
		return fmt.Errorf("empty WhatsApp target")
	}
	return k.rt.comms.Send("whatsapp-main", target, content)
}

func (k *loopKit) ReadWhatsApp(ctx context.Context, chat string, limit int) (string, error) {
	tool := &builtin.WhatsAppReadTool{WacliPath: k.wacliPath, Store: k.rt.store}
	in := map[string]any{}
	if strings.TrimSpace(chat) != "" {
		in["chat"] = chat
	}
	if limit > 0 {
		in["limit"] = limit
	}
	res, err := tool.Execute(ctx, in)
	if err != nil {
		return "", err
	}
	if res.IsError {
		return "", fmt.Errorf("whatsapp.read: %s", res.Error)
	}
	b, _ := json.Marshal(res.Output)
	return string(b), nil
}

func (k *loopKit) HTTP(ctx context.Context, method, url string, headers map[string]string, body string) (string, int, error) {
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(method), url, strings.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	for hk, hv := range headers {
		req.Header.Set(hk, hv)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8MB safety cap
	return string(b), resp.StatusCode, nil
}

// Config reads an install-time value from the environment, namespaced per loop:
// KARMAX_LOOP_<LOOPNAME>_<KEY> (non-alnum chars uppercased to '_').
func (k *loopKit) Config(key string) string {
	return os.Getenv("KARMAX_LOOP_" + envSanitize(k.loopName) + "_" + envSanitize(key))
}

func (k *loopKit) Logf(format string, args ...any) {
	k.rt.log.Info("[loop:" + k.loopName + "] " + fmt.Sprintf(format, args...))
}

func loopToolField(res tools.ToolResult, key string) string {
	if m, ok := res.Output.(map[string]any); ok {
		if s, ok := m[key].(string); ok {
			return s
		}
	}
	if s, ok := res.Output.(string); ok {
		return s
	}
	return ""
}

func envSanitize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}
