// Package entrypoint names how a Claude Code process was started, as the request's
// User-Agent and the transcript's "entrypoint" field record it. Anthropic's five-hour
// meter charges Agent SDK entrypoints more per token than interactive ones.
package entrypoint

import (
	"regexp"
	"strings"
)

var userAgent = regexp.MustCompile(`(?i)^claude-cli/\S+\s+\(external,\s*([^,)]+)`)

// FromUserAgent is the entrypoint in `claude-cli/<version> (external, <entrypoint>...)`, or "".
func FromUserAgent(ua string) string {
	m := userAgent.FindStringSubmatch(strings.TrimSpace(ua))
	if m == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(m[1]))
}

// AgentSDK reports whether the entrypoint is `claude -p` or an Agent SDK.
func AgentSDK(entrypoint string) bool {
	switch entrypoint {
	case "sdk-cli", "sdk-ts", "sdk-py":
		return true
	}
	return false
}

// Weight is what one token of this entrypoint costs on the five-hour meter, in interactive tokens.
func Weight(entrypoint string, factor float64) float64 {
	if AgentSDK(entrypoint) {
		return factor
	}
	return 1
}
