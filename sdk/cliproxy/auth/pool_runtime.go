package auth

import "strings"

// ClaudeRuntime keeps the temporary API-credit bridge's routing decision in the pool.
// An SDK client must switch to interactive Claude Code before using a subscription.
func (m *Manager) ClaudeRuntime(prefix string) (runtime, reason string) {
	for _, row := range m.PoolSnapshot(nil).Accounts {
		if row.Known && row.Usable && row.Comfortable {
			return "interactive", "subscription headroom is available"
		}
	}
	if prefix == "" {
		return "interactive", "no API credential prefix was selected"
	}
	for _, credential := range m.List() {
		if credential.Provider != "claude" || credential.Prefix != prefix || credential.Attributes["api_key"] == "" || credential.Disabled {
			continue
		}
		if credential.LastError != nil && strings.Contains(strings.ToLower(credential.LastError.Message), "credit balance is too low") {
			continue
		}
		return "sdk", "API credit credential is available"
	}
	return "interactive", "API credit credential is unavailable"
}
