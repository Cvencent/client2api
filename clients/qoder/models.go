package qoder

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"client2api/internal/core"
)

// models.go owns the catalogue.
//
// The vendor publishes its own list at
// GET https://gateway.qoder.com.cn/algo/api/v2/model/list?Encode=1, behind the
// same COSY signature the chat stream needs (see cosy.go).  The live list is
// grouped by who consumes it -- chat, developer, assistant, inline, quest,
// qwork, experts, qwake, app -- and the keys repeat across groups, so the
// catalogue keeps the "chat" group and de-duplicates by key.
//
// builtinModels below is the fallback: it is what the module answers with
// before the first successful fetch, and when the vendor cannot be reached, so
// the model picker is never empty.  The ids and context windows in it are the
// vendor's own, read from the live list, not guesses.

// defaultTestModel is the model a connectivity probe asks for.  "auto" is the
// vendor's own default alias and routes to whatever it currently considers
// current, which is exactly what a probe wants.
const defaultTestModel = "auto"

// modelInfo is one catalogue entry, vendor-spelled.
type modelInfo struct {
	ID            string
	DisplayName   string
	ContextLength int
	MaxOutput     int
	PriceFactor   *float64
	OriginalPrice *float64
	Promotion     *modelPromotion
	Reasoning     bool
	Vision        bool
	Default       bool
	Free          bool
	New           bool
}

// modelPromotion is the vendor's off-peak campaign. `active` is only a
// snapshot: when the window is present, promotionActiveNow recomputes it from
// the current UTC+8 wall clock.
type modelPromotion struct {
	Active                     bool     `json:"active"`
	DiscountFactor             *float64 `json:"discount_factor"`
	BeforePromotionPriceFactor *float64 `json:"before_promotion_price_factor"`
	WindowStart                string   `json:"window_start"`
	WindowEnd                  string   `json:"window_end"`
}

func floatPtr(v float64) *float64 { return &v }

func promotionActiveNow(p *modelPromotion, now time.Time) bool {
	if p == nil {
		return false
	}
	start, okStart := qoderClockMinutes(p.WindowStart)
	end, okEnd := qoderClockMinutes(p.WindowEnd)
	if !okStart || !okEnd {
		return p.Active
	}
	// Qoder publishes the window in UTC+8, so compute against that wall clock
	// instead of the host's local timezone.
	utc8 := now.UTC().Add(8 * time.Hour)
	minutes := utc8.Hour()*60 + utc8.Minute()
	if start <= end {
		return minutes >= start && minutes < end
	}
	return minutes >= start || minutes < end
}

func qoderClockMinutes(s string) (int, bool) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, false
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return 0, false
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// qoderBillingValue renders the same three states as the vendor picker: free,
// an in-window original->discounted pair, or a plain factor.
func qoderBillingValue(m modelInfo, now time.Time) (base, current *float64, label string, free bool) {
	if m.PriceFactor == nil {
		return nil, nil, "", false
	}
	if *m.PriceFactor == 0 {
		return floatPtr(0), floatPtr(0), "", true
	}
	p := m.Promotion
	if p != nil && p.DiscountFactor != nil && p.BeforePromotionPriceFactor != nil {
		before := *p.BeforePromotionPriceFactor
		if before > 0 {
			if promotionActiveNow(p, now) {
				current := math.Round(before*(*p.DiscountFactor)*10000) / 10000
				return floatPtr(before), floatPtr(current), "错峰折扣", false
			}
			return floatPtr(before), floatPtr(before), "", false
		}
	}
	if m.OriginalPrice != nil && *m.OriginalPrice > 0 && *m.PriceFactor != *m.OriginalPrice {
		return m.OriginalPrice, m.PriceFactor, "", false
	}
	return m.PriceFactor, m.PriceFactor, "", false
}

// builtinModels is the fallback catalogue: the chat group of the vendor's list
// as this module last confirmed it.
func builtinModels() []modelInfo {
	return []modelInfo{
		{ID: "auto", DisplayName: "Auto", ContextLength: 200000, PriceFactor: floatPtr(0.5), Reasoning: true, Vision: true, Default: true},
		{ID: "qmodel_38max", DisplayName: "Qwen3.8-Max", ContextLength: 180000, PriceFactor: floatPtr(0.2), Promotion: &modelPromotion{Active: true, DiscountFactor: floatPtr(0.4), BeforePromotionPriceFactor: floatPtr(0.5), WindowStart: "22:00", WindowEnd: "08:00"}, Reasoning: true, Vision: true, Free: true, New: true},
		{ID: "qfmodel", DisplayName: "Qwen3.8-Flash", ContextLength: 180000, PriceFactor: floatPtr(0), OriginalPrice: floatPtr(0.1), Reasoning: true, Vision: true, New: true},
		{ID: "qmodel_latest", DisplayName: "Qwen3.7-Max", ContextLength: 180000, PriceFactor: floatPtr(0.1), OriginalPrice: floatPtr(0.5), Promotion: &modelPromotion{Active: true, DiscountFactor: floatPtr(0.2), BeforePromotionPriceFactor: floatPtr(0.5), WindowStart: "22:00", WindowEnd: "08:00"}, Reasoning: true, Vision: true},
		{ID: "qmodel", DisplayName: "Qwen3.7-Plus", ContextLength: 180000, PriceFactor: floatPtr(0.04), Promotion: &modelPromotion{Active: true, DiscountFactor: floatPtr(0.4), BeforePromotionPriceFactor: floatPtr(0.1), WindowStart: "22:00", WindowEnd: "08:00"}, Reasoning: true, Vision: true},
		{ID: "q37fmodel", DisplayName: "Qwen3.7-Flash", ContextLength: 180000, PriceFactor: floatPtr(0.1), Reasoning: true, Vision: true},
		{ID: "dmodel", DisplayName: "DeepSeek-V4-Pro", ContextLength: 96000, PriceFactor: floatPtr(0.5)},
		{ID: "dfmodel", DisplayName: "DeepSeek-Flash", ContextLength: 180000, PriceFactor: floatPtr(0.1)},
		{ID: "gmodel", DisplayName: "GLM-5.3", ContextLength: 180000, PriceFactor: floatPtr(0.8)},
		{ID: "gfmodel", DisplayName: "GLM-5.3-Flash", ContextLength: 1000000, PriceFactor: floatPtr(0.1)},
		{ID: "gm51model", DisplayName: "GLM-5.2", ContextLength: 180000, PriceFactor: floatPtr(0.6)},
		{ID: "kmodel_latest", DisplayName: "Kimi-K3", ContextLength: 180000, PriceFactor: floatPtr(1.4)},
		{ID: "kmodel", DisplayName: "Kimi-K2.8-Preview", ContextLength: 180000, PriceFactor: floatPtr(0.8)},
		{ID: "mmodel", DisplayName: "MiniMax-M2.7", ContextLength: 180000, PriceFactor: floatPtr(0.2)},
	}
}

// builtinModelIDs is the bare id list the fallback catalogue renders.
func builtinModelIDs() []string {
	infos := builtinModels()
	out := make([]string, 0, len(infos))
	for _, m := range infos {
		out = append(out, m.ID)
	}
	return out
}

// fallbackModels is the catalogue served before the first successful vendor
// refresh, and whenever a refresh cannot reach Qoder. An explicit config list
// remains an operator override; otherwise the compiled-in chat group is used.
func (c *Client) fallbackModels() []core.Model {
	if c == nil || len(c.cfg.Models) == 0 {
		return catalogue(builtinModels())
	}
	infos := make([]modelInfo, 0, len(c.cfg.Models))
	for _, id := range c.cfg.modelIDs() {
		infos = append(infos, modelInfo{ID: id, DisplayName: id})
	}
	return catalogue(infos)
}

// cachedModels returns a snapshot of the live catalogue. The list is immutable
// once stored, so sharing the slice is safe.
func (c *Client) cachedModels() ([]core.Model, time.Time) {
	c.modelMu.Lock()
	defer c.modelMu.Unlock()
	return c.models, c.modelsAt
}

// modelsRetryInterval prevents a failed background fetch from being retried on
// every Status poll.
const modelsRetryInterval = 2 * time.Minute

// RefreshModels fetches Qoder's current chat catalogue synchronously.
//
// A failure never empties the model picker: the previous live list, or the
// compiled-in fallback, is returned alongside the error.
func (c *Client) RefreshModels(ctx context.Context) ([]core.Model, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	previous, _ := c.cachedModels()
	fallback := previous
	if len(fallback) == 0 {
		fallback = c.fallbackModels()
	}

	rctx, cancel := context.WithTimeout(ctx, c.cfg.modelsTimeout())
	defer cancel()
	infos, err := c.fetchModels(rctx)
	if err != nil {
		c.logf("qoder: model list refresh: %s", redactErr(err))
		return fallback, fmt.Errorf("qoder: refresh models: %s", redactErr(err))
	}
	if len(infos) == 0 {
		return fallback, errors.New("qoder: refresh models: upstream returned an empty catalogue")
	}

	models := catalogue(infos)
	now := time.Now()
	c.modelMu.Lock()
	c.models = models
	c.modelsAt = now
	c.modelsAttemptAt = now
	c.modelMu.Unlock()
	return models, nil
}

// refreshModelsAsync keeps Models cheap while still discovering a catalogue
// change without the operator pressing refresh.
func (c *Client) refreshModelsAsync() {
	c.modelMu.Lock()
	if c.modelsLoading || (!c.modelsAttemptAt.IsZero() && time.Since(c.modelsAttemptAt) < modelsRetryInterval) {
		c.modelMu.Unlock()
		return
	}
	c.modelsLoading = true
	c.modelsAttemptAt = time.Now()
	c.modelMu.Unlock()

	core.GoSafe("qoder model refresh", func(msg string) { c.logf("qoder: %s", msg) }, func() {
		defer func() {
			c.modelMu.Lock()
			c.modelsLoading = false
			c.modelMu.Unlock()
		}()

		ctx, cancel := context.WithTimeout(context.Background(), c.cfg.modelsTimeout())
		defer cancel()
		infos, err := c.fetchModels(ctx)
		if err != nil {
			c.logf("qoder: model list: %s", redactErr(err))
			return
		}
		if len(infos) == 0 {
			return
		}
		models := catalogue(infos)
		c.modelMu.Lock()
		c.models = models
		c.modelsAt = time.Now()
		c.modelMu.Unlock()
	})
}

// fetchModels tries the signed catalogue route with the pool's normal account
// order until one credential succeeds.
func (c *Client) fetchModels(ctx context.Context) ([]modelInfo, error) {
	candidates := c.store.candidates(nowUTC())
	if len(candidates) == 0 {
		return nil, core.ErrNotConfigured
	}
	var lastErr error
	for i := 0; i < len(candidates) && i < maxChatAttempts; i++ {
		// A CLI-only account has no device token to sign the catalogue request
		// with, and blaming it for the resulting 401 would park a working
		// account.
		if candidates[i].cliOnly() {
			continue
		}
		infos, err := c.fetchModelList(ctx, candidates[i])
		if err == nil {
			return infos, nil
		}
		lastErr = err
		if callerGone(err) {
			return nil, err
		}
	}
	if lastErr == nil {
		lastErr = core.ErrNotConfigured
	}
	return nil, lastErr
}

// catalogue renders model info as the core catalogue.
func catalogue(infos []modelInfo) []core.Model {
	out := make([]core.Model, 0, len(infos))
	for _, m := range infos {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		name := m.DisplayName
		if name == "" {
			name = id
		}
		extra := map[string]any{
			"platform":     "qoder",
			"display_name": name,
		}
		if m.ContextLength > 0 {
			// context_length is the field the panel's per-model editor reads
			// and rewrites, so the vendor's own window lands there verbatim.
			extra["context_length"] = m.ContextLength
			extra["max_input_tokens"] = m.ContextLength
		}
		if m.MaxOutput > 0 {
			extra["max_output_tokens"] = m.MaxOutput
		}
		if m.Reasoning {
			extra["reasoning"] = true
		}
		if m.Vision {
			extra["vision"] = true
		}
		if m.Default {
			extra["default"] = true
		}
		if m.Free {
			extra["free"] = true
		}
		if base, current, label, free := qoderBillingValue(m, time.Now()); base != nil {
			extra["credits"] = strconv.FormatFloat(*base, 'f', -1, 64)
			if free {
				extra["credits"] = "免费"
			} else if current != nil && *current != *base {
				extra["promo_credits"] = strconv.FormatFloat(*current, 'f', -1, 64)
				extra["promo_factor"] = *current / *base
			}
			if label != "" {
				extra["promo_label"] = label
			}
		}
		out = append(out, core.Model{ID: id, OwnedBy: "qoder", Extra: extra})
	}
	return out
}

// modelIDsFrom renders just the ids of a catalogue.
func modelIDsFrom(models []core.Model) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

// gatewayModelList is the shape of GET /algo/api/v2/model/list.  Only the
// "chat" group is modelled: it is the only group a chat gateway can serve, and
// the other groups repeat its keys.
type gatewayModelList struct {
	Chat []gatewayModel `json:"chat"`
}

type gatewayModel struct {
	Key             string          `json:"key"`
	DisplayName     string          `json:"display_name"`
	Enable          *bool           `json:"enable"`
	IsReasoning     bool            `json:"is_reasoning"`
	IsVL            bool            `json:"is_vl"`
	IsDefault       bool            `json:"is_default"`
	IsFree          bool            `json:"is_free"`
	IsNew           bool            `json:"is_new"`
	MaxInputTokens  int             `json:"max_input_tokens"`
	MaxOutputTokens int             `json:"max_output_tokens"`
	PriceFactor     *float64        `json:"price_factor"`
	OriginalPrice   *float64        `json:"original_price_factor"`
	BeforePrice     *float64        `json:"before_promotion_price_factor"`
	Promotion       *modelPromotion `json:"promotion"`
}

// chatModels turns the vendor's chat group into a catalogue, dropping disabled
// entries and de-duplicating by key while keeping the vendor's order.
func (l gatewayModelList) chatModels() []modelInfo {
	out := make([]modelInfo, 0, len(l.Chat))
	seen := map[string]bool{}
	for _, m := range l.Chat {
		id := strings.TrimSpace(m.Key)
		if id == "" || seen[id] {
			continue
		}
		if m.Enable != nil && !*m.Enable {
			continue
		}
		seen[id] = true
		out = append(out, modelInfo{
			ID:            id,
			DisplayName:   m.DisplayName,
			ContextLength: m.MaxInputTokens,
			MaxOutput:     m.MaxOutputTokens,
			Reasoning:     m.IsReasoning,
			Vision:        m.IsVL,
			Default:       m.IsDefault,
			Free:          m.IsFree,
			New:           m.IsNew,
			PriceFactor:   m.PriceFactor,
			OriginalPrice: firstNonNilFloat(m.OriginalPrice, m.BeforePrice),
			Promotion:     m.Promotion,
		})
	}
	return out
}

func firstNonNilFloat(values ...*float64) *float64 {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

// ModelMaxOutputTokens implements core.ModelLimitsProvider.  The catalogue is
// already cached for the panel, so this never performs a network call on the
// request path: a cold cache answers ok=false and the gateway sends no cap.
func (c *Client) ModelMaxOutputTokens(ctx context.Context, model string) (int, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	_ = ctx
	models, _ := c.cachedModels()
	if len(models) == 0 {
		models = c.fallbackModels()
	}
	return core.OutputLimitFor(models, model)
}
