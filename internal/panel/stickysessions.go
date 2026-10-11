package panel

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"client2api/internal/core"
	"client2api/internal/gateway"
)

type stickySessionView struct {
	Key           string     `json:"key"`
	Client        string     `json:"client,omitempty"`
	Account       string     `json:"account,omitempty"`
	Platform      string     `json:"platform,omitempty"`
	AccountBound  bool       `json:"account_bound"`
	PlatformBound bool       `json:"platform_bound"`
	LastActive    *time.Time `json:"last_active,omitempty"`
}

type stickySessionRequest struct {
	Client string `json:"client"`
	Key    string `json:"key"`
}

// handleStickySessions lists the account and platform bindings the gateway is
// currently holding.  It is deliberately a separate aggregate route: the
// account tables live in modules, while the platform table lives in gateway.
func (p *panel) handleStickySessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": p.stickySessions()})
}

func (p *panel) stickySessions() []stickySessionView {
	rows := []stickySessionView{}
	accountKeys := map[string]bool{}
	platforms := map[string]gateway.StickyRoute{}
	if p != nil && p.opts.SessionPlatforms != nil {
		for _, route := range p.opts.SessionPlatforms.List() {
			platforms[route.Key] = route
		}
	}
	if p != nil && p.opts.Registry != nil {
		for _, c := range p.opts.Registry.All() {
			binder, ok := core.AsConversationBinder(c)
			if !ok {
				continue
			}
			for _, binding := range binder.ConversationBindings() {
				if strings.TrimSpace(binding.Key) == "" {
					continue
				}
				row := stickySessionView{
					Key:          binding.Key,
					Client:       c.Name(),
					Account:      binding.Account,
					AccountBound: true,
					LastActive:   timePtr(binding.LastActive),
				}
				if route, ok := platforms[binding.Key]; ok {
					row.Platform = route.Platform
					row.PlatformBound = true
					if row.LastActive == nil || route.LastSeen.After(*row.LastActive) {
						row.LastActive = timePtr(route.LastSeen)
					}
				}
				rows = append(rows, row)
				accountKeys[binding.Key] = true
			}
		}
	}
	for key, route := range platforms {
		if accountKeys[key] {
			continue
		}
		rows = append(rows, stickySessionView{
			Key:           key,
			Platform:      route.Platform,
			PlatformBound: true,
			LastActive:    timePtr(route.LastSeen),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		left, right := rows[i].LastActive, rows[j].LastActive
		if left == nil && right != nil {
			return false
		}
		if left != nil && right == nil {
			return true
		}
		if left != nil && right != nil && !left.Equal(*right) {
			return left.After(*right)
		}
		if rows[i].Key != rows[j].Key {
			return rows[i].Key < rows[j].Key
		}
		return rows[i].Client < rows[j].Client
	})
	return rows
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func (p *panel) handleStickySessionUnbind(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var in stickySessionRequest
	if !decodeJSON(w, r, &in) {
		return
	}
	in.Key = strings.TrimSpace(in.Key)
	in.Client = strings.TrimSpace(in.Client)
	if in.Key == "" {
		writeErr(w, http.StatusBadRequest, "key is required")
		return
	}
	if in.Client == "" {
		writeErr(w, http.StatusBadRequest, "client is required")
		return
	}
	binder, ok, status := p.stickyBinder(in.Client)
	if !ok {
		writeErr(w, status, stickyBinderError(in.Client, status))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"key":               in.Key,
		"client":            in.Client,
		"forgotten_account": binder.UnbindConversation(in.Key),
	})
}

func (p *panel) handleStickySessionSwitch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var in stickySessionRequest
	if !decodeJSON(w, r, &in) {
		return
	}
	in.Key = strings.TrimSpace(in.Key)
	in.Client = strings.TrimSpace(in.Client)
	if in.Key == "" {
		writeErr(w, http.StatusBadRequest, "key is required")
		return
	}
	forgottenAccount := false
	if in.Client != "" {
		binder, ok, status := p.stickyBinder(in.Client)
		if !ok {
			writeErr(w, status, stickyBinderError(in.Client, status))
			return
		}
		forgottenAccount = binder.UnbindConversation(in.Key)
	}
	forgottenPlatform := false
	if p != nil && p.opts.SessionPlatforms != nil {
		forgottenPlatform = p.opts.SessionPlatforms.Forget(in.Key)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"key":                in.Key,
		"client":             in.Client,
		"forgotten_account":  forgottenAccount,
		"forgotten_platform": forgottenPlatform,
	})
}

func (p *panel) stickyBinder(name string) (core.ConversationBinder, bool, int) {
	if p == nil || p.opts.Registry == nil {
		return nil, false, http.StatusServiceUnavailable
	}
	c, ok := p.opts.Registry.Get(name)
	if !ok {
		return nil, false, http.StatusNotFound
	}
	binder, ok := core.AsConversationBinder(c)
	if !ok {
		return nil, false, http.StatusNotImplemented
	}
	return binder, true, http.StatusOK
}

func stickyBinderError(name string, status int) string {
	switch status {
	case http.StatusNotFound:
		return "no such client: " + name
	case http.StatusNotImplemented:
		return name + " has no conversation table to manage"
	default:
		return "no registry"
	}
}
