package auth

import "strings"

// ClaudeRuntime keeps the API-credit bridge's routing decision in the pool: any credit with
// balance runs as an SDK client, and only when every credit is empty does it switch to
// interactive Claude Code on a subscription.
func (m *Manager) ClaudeRuntime(prefix string) (runtime, reason string) {
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
	return "interactive", "every API credit is empty"
}
