package auth

import (
	"context"
	"maps"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// creditModel names a bare Claude model under the prefix of the highest-priority API credit
// that can serve it now, so a caller naming a plain model spends credit before any plan.
func (m *Manager) creditModel(providers []string, model string) (string, bool) {
	model = strings.TrimSpace(model)
	if model == "" || strings.Contains(model, "/") || !containsProvider(m.normalizeProviders(providers), "claude") {
		return "", false
	}
	reg, now := registry.GetGlobalRegistry(), time.Now()
	best, bestPriority := "", 0
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, a := range m.auths {
		if a == nil || a.Provider != "claude" || a.Prefix == "" || a.Attributes["api_key"] == "" {
			continue
		}
		credit := a.Prefix + "/" + model
		if !reg.ClientSupportsModel(a.ID, canonicalModelKey(credit)) {
			continue
		}
		if blocked, _, _ := isAuthBlockedForModel(a, credit, now); blocked {
			continue
		}
		if blocked, _, _ := isAuthBlockedForModel(a, model, now); blocked {
			continue
		}
		if priority := authPriority(a); best == "" || priority > bestPriority {
			best, bestPriority = credit, priority
		}
	}
	return best, best != ""
}

// creditFirst runs a bare Claude request on the API credits, and on the plans only once no
// credit can serve it. An error while a credit still has balance is the request's own.
func creditFirst[T any](ctx context.Context, m *Manager, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, run func(cliproxyexecutor.Request, cliproxyexecutor.Options) (T, error)) (T, error) {
	model := authSelectionModelFromOptions(opts, req.Model)
	credit, ok := m.creditModel(providers, model)
	if !ok {
		return run(req, opts)
	}
	creditReq, creditOpts := req, opts
	creditOpts.Metadata = maps.Clone(opts.Metadata)
	if _, selected := creditOpts.Metadata[cliproxyexecutor.AuthSelectionModelMetadataKey]; selected {
		creditOpts.Metadata[cliproxyexecutor.AuthSelectionModelMetadataKey] = credit
	} else {
		creditReq.Model = credit
	}
	out, err := run(creditReq, creditOpts)
	if err == nil || ctx.Err() != nil {
		return out, err
	}
	if _, stillFunded := m.creditModel(providers, model); stillFunded {
		return out, err
	}
	return run(req, opts)
}
