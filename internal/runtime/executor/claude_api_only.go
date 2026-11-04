package executor

import (
	"net/http"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func requireClaudeAPIKey(auth *cliproxyauth.Auth, opts cliproxyexecutor.Options) error {
	key, _ := claudeCreds(auth)
	if opts.Headers.Get("X-Proxy-API-Only") == "1" && claudeCredentialUsesOAuth(auth, key) {
		return statusErr{code: http.StatusConflict, msg: "interactive_claude_required: API-only requests cannot use a subscription credential"}
	}
	return nil
}
