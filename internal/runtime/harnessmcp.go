package runtime

import "go.uber.org/zap"

// harnessMCPConfig is harness.mcp_servers as the --mcp-config every session
// is limited to, or "" — the CLI's own servers — when unset or unusable.
func (rt *KarmaxRuntime) harnessMCPConfig() string {
	cfg, err := rt.cfg.Harness.MCPConfigJSON()
	if err != nil {
		rt.log.Warn("harness: ignoring mcp_servers", zap.Error(err))
		return ""
	}
	return cfg
}
