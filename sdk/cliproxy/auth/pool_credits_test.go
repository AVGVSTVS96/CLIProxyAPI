package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const lowCredit = "Your credit balance is too low to access the Anthropic API."

// creditPool registers the auths with the live selector and an executor that answers 429 from a
// plan listed in dry and a low balance from a key listed in empty.
func creditPool(t *testing.T, auths ...*Auth) (*Manager, map[string]bool, map[string]bool) {
	t.Helper()
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })
	resetPool(t)
	m := NewManager(nil, NewSessionAffinitySelector(&QuotaSelector{}), nil)
	reg := registry.GetGlobalRegistry()
	for _, a := range auths {
		reg.RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: "claude-opus-5-5"}})
		t.Cleanup(func() { reg.UnregisterClient(a.ID) })
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	dry, empty := map[string]bool{}, map[string]bool{}
	m.RegisterExecutor(&mockCustomErrorExecutor{identifier: "claude", executeFn: func(_ context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
		if dry[a.ID] {
			return cliproxyexecutor.Response{}, customStatusError{code: 429, msg: `{"type":"error","error":{"type":"rate_limit_error","message":"limit"}}`}
		}
		if empty[a.ID] {
			return cliproxyexecutor.Response{}, customStatusError{code: 400, msg: `{"type":"error","error":{"type":"invalid_request_error","message":"` + lowCredit + `"}}`}
		}
		return cliproxyexecutor.Response{Payload: []byte(a.ID)}, nil
	}})
	return m, dry, empty
}

func creditKey(id string) *Auth {
	rules := []internalconfig.RequestScopedErrorRule{{Status: 400, Match: []string{lowCredit}, Action: "continue-and-cooldown"}}
	return &Auth{ID: id, Provider: "claude", Status: StatusActive, Attributes: map[string]string{"api_key": id + "-key"},
		Metadata: map[string]any{"request_scoped_errors": rules}}
}

func creditPlan(id string) *Auth {
	return &Auth{ID: id, Provider: "claude", Status: StatusActive, Metadata: map[string]any{"account_uuid": id, "email": id + "@example.com"}}
}

// send runs one request of session from a Claude Code entrypoint ("" for a caller that isn't Claude Code).
func send(m *Manager, session, via string) string {
	headers := http.Header{}
	if via != "" {
		headers.Set("User-Agent", "claude-cli/2.1.317 (external, "+via+")")
		headers.Set("X-Claude-Code-Session-Id", session)
	}
	resp, err := m.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "claude-opus-5-5"}, cliproxyexecutor.Options{Headers: headers})
	if err != nil {
		return "error"
	}
	return string(resp.Payload)
}

func maxOut(id string, used float64) {
	claudePool.mu.Lock()
	claudePool.account(id).reading = PoolReading{FiveHour: &PoolWindow{Used: 0}, Week: &PoolWindow{Used: used, ResetsAt: time.Now().Add(48 * time.Hour).UnixMilli()}, At: time.Now().UnixMilli()}
	claudePool.mu.Unlock()
}

func TestFreeCreditTakesAgentSDKWorkOnlyOnceEveryPlanIsMaxedOut(t *testing.T) {
	m, _, _ := creditPool(t, creditKey("credit"), creditPlan("plan"))
	one, two := "11111111-2222-3333-4444-555555555555", "21111111-2222-3333-4444-555555555555"
	if got := send(m, one, "sdk-cli"); got != "plan" {
		t.Fatalf("with a plan that has room, claude -p went to %s", got)
	}
	maxOut("plan", 100)
	if got := send(m, one, "sdk-cli"); got != "credit" {
		t.Fatalf("with every plan maxed out, a claude -p session bound to the plan went to %s, want free credit", got)
	}
	if got := send(m, two, "sdk-ts"); got != "credit" {
		t.Fatalf("with every plan maxed out, a new Agent SDK session went to %s, want free credit", got)
	}
	if got := send(m, "", ""); got != "credit" {
		t.Fatalf("with every plan maxed out, an API caller went to %s, want free credit", got)
	}
	maxOut("plan", 40)
	if got := send(m, one, "sdk-cli"); got != "plan" {
		t.Fatalf("once the plan had room again, the session stayed on %s", got)
	}
}

func TestFreeCreditCatchesWorkOnceThePlansAnswer429(t *testing.T) {
	m, dry, _ := creditPool(t, creditKey("credit"), creditPlan("plan"))
	session := "11111111-2222-3333-4444-555555555555"
	dry["plan"] = true
	if got := send(m, session, "sdk-cli"); got != "credit" {
		t.Fatalf("the only plan answered 429 and claude -p went to %s, want free credit", got)
	}
	delete(dry, "plan")
	if _, _, err := m.ResetQuota(context.Background(), "plan"); err != nil {
		t.Fatal(err)
	}
	if got := send(m, session, "sdk-cli"); got != "plan" {
		t.Fatalf("after the plan came back the session stayed on %s", got)
	}
}

// Anthropic doesn't count free credit for interactive Claude Code, and a session that names
// itself (hex, microscope, t3-code) doesn't say it is the Agent SDK or claude -p.
func TestInteractiveSessionsNeverGetFreeCredit(t *testing.T) {
	m, dry, _ := creditPool(t, creditKey("credit"), creditPlan("plan"))
	maxOut("plan", 100)
	for _, via := range []string{"cli", "hex", "t3-code"} {
		if got := send(m, "31111111-2222-3333-4444-555555555555", via); got != "plan" {
			t.Fatalf("an interactive %s session went to %s with every plan maxed out, want the plan", via, got)
		}
	}
	dry["plan"] = true
	if got := send(m, "41111111-2222-3333-4444-555555555555", "cli"); got != "error" {
		t.Fatalf("an interactive session went to %s once every plan refused, want none", got)
	}
}

func TestFreeCreditMovesToTheNextKeyOnceOneIsEmpty(t *testing.T) {
	m, _, empty := creditPool(t, creditKey("first"), creditKey("second"))
	empty["first"] = true
	if got := send(m, "", ""); got != "second" {
		t.Fatalf("the first key was empty and the request went to %s, want second", got)
	}
	empty["second"] = true
	if got := send(m, "", ""); got != "error" {
		t.Fatalf("every key is empty but the request was served by %s", got)
	}
}
