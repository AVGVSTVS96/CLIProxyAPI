// Package usage collects token usage from coding-agent logs on every machine,
// prices it, and serves it to the dashboard's Usage, Live and Accounts pages.
package usage

// Event is one request's accounting, in the same JSON shape token-tracker's
// UsageEvent uses, so its history imports as is and ids match across machines.
type Event struct {
	Source             string   `json:"source,omitempty"`
	ResponseID         string   `json:"responseId,omitempty"`
	UpstreamRequestID  string   `json:"upstreamRequestId,omitempty"`
	AccountID          string   `json:"accountId,omitempty"`
	Failed             bool     `json:"failed,omitempty"`
	ID                 string   `json:"id"`
	Timestamp          string   `json:"timestamp"`
	Harness            string   `json:"harness"`
	Model              string   `json:"model"`
	RawModel           string   `json:"rawModel,omitempty"`
	Project            string   `json:"project"`
	Repository         *string  `json:"repository"`
	SessionID          string   `json:"sessionId"`
	InputTokens        int64    `json:"inputTokens"`
	OutputTokens       int64    `json:"outputTokens"`
	CacheReadTokens    int64    `json:"cacheReadTokens"`
	CacheWriteTokens   int64    `json:"cacheWriteTokens"`
	UnclassifiedTokens int64    `json:"unclassifiedTokens,omitempty"`
	ReasoningTokens    int64    `json:"reasoningTokens"`
	CostUSD            float64  `json:"costUsd"`
	ReportedCostUSD    *float64 `json:"reportedCostUsd,omitempty"`
	CostKnown          bool     `json:"costKnown"`
	ServiceTier        *string  `json:"serviceTier,omitempty"`
	CacheWrite1hTokens *int64   `json:"cacheWrite1hTokens,omitempty"`
	Requests           *int64   `json:"requests,omitempty"`
	DeviceID           string   `json:"deviceId,omitempty"`
	SessionTitle       string   `json:"sessionTitle,omitempty"`
	ProjectName        string   `json:"projectName,omitempty"`
	T3ThreadID         string   `json:"t3ThreadId,omitempty"`
	T3ThreadURL        string   `json:"t3ThreadUrl,omitempty"`
	// Entrypoint is how the Claude Code process was started: cli, sdk-cli (claude -p), sdk-ts, or a product's own name.
	Entrypoint string `json:"entrypoint,omitempty"`
}

// Device is a machine whose events the store holds.
type Device struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	SyncedAt *int64 `json:"syncedAt"`
}
