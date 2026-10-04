package workbuddy

import (
	"context"
	"encoding/json"
	"net/http"
)

// School-activity endpoints: the voucher-code read plus the three miniprogram
// event builders the reference keeps in internal/upstream/school.go (MIT).
//
// Scope note: this module already owns the *default* (desktop) shapes of the
// playbook and chat events (tasks.go desktopPlaybookPromptSequence, mpChatEvent,
// reportMPEvent) and the miniprogram chat loop (runMPChatTask).  What was
// missing is the school voucher read and the three miniprogram event builders
// below, which is what this file adds.  Nothing here re-implements the report
// transport: reportMPEvent and mpChatEvent stay the single owners.

// schoolBase is the portal activity prefix used by the school campaign.
const schoolBase = "/portal/activity/school"

// miniExtVersion is the extension version the school events advertise.  It
// deliberately differs from the miniprogram fingerprint's 2.4.0: the vendor
// measured that the school activity only scores events carrying 2.2.8.
const miniExtVersion = "2.2.8"

// schoolJSON performs one school-portal call.  The envelope is judged like the
// reference's doJSON (a non-zero business code is an *Error) and the unwrapped
// payload is unmarshalled into out when out is not nil.
func (c *Client) schoolJSON(ctx context.Context, a *Auth, method, path string, payload any, out any) error {
	var body []byte
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = raw
	}
	env, err := c.taskJSON(ctx, method, c.up.billingBase(a)+schoolBase+path, body, func(req *http.Request) {
		c.up.BillingHeaders(req, a)
	})
	if err != nil {
		return err
	}
	if err := envelopeError(http.StatusOK, env); err != nil {
		return err
	}
	return decodePayload(env.Data, out)
}

// SchoolVoucher is one prize the account has won in the school activity.  Code
// is the redemption code the panel has to be able to list.
type SchoolVoucher struct {
	GrantID   int64  `json:"grant_id"`
	DrawUUID  string `json:"draw_uuid,omitempty"`
	SKUCode   string `json:"sku_code,omitempty"`
	PrizeName string `json:"prize_name,omitempty"`
	Code      string `json:"code"`
	ValidFrom string `json:"valid_from,omitempty"`
	ValidTo   string `json:"valid_to,omitempty"`
	GrantedAt string `json:"granted_at,omitempty"`
}

// SchoolVouchers lists the account's school-activity vouchers, newest first as
// the vendor returns them.
func (c *Client) SchoolVouchers(ctx context.Context, a *Auth) ([]SchoolVoucher, error) {
	var out struct {
		Items []SchoolVoucher `json:"items"`
	}
	if err := c.schoolJSON(ctx, a, http.MethodGet, "/vouchers", nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// MiniExpertUseEvent is the miniprogram `expert_actual_use` event.
//
// It carries no conversationId and no activityId on purpose: the school
// activity scores this one as a standalone "the operator really used an expert"
// signal.  expertID must be a real market `ex_` id, otherwise the server
// accepts the report and scores nothing.
func MiniExpertUseEvent(expertID, expertName, expertType string) map[string]any {
	if expertType == "" {
		expertType = "agent"
	}
	if expertName == "" {
		expertName = expertID
	}
	return map[string]any{
		"eventCode":      "expert_actual_use",
		"reportDelay":    0,
		"extVersion":     miniExtVersion,
		"source":         "mini_program",
		"id":             expertID,
		"name":           expertID,
		"expertTitle":    expertName,
		"type":           "send_message",
		"characterCount": 12,
		"expertType":     expertType,
	}
}

// MiniChatModelEvent is the miniprogram chat event that also declares which
// model the chat asked for.  It is the carrier for the "use GLM-5.2" chore.
func MiniChatModelEvent(conversationID, modelID, modelName string) map[string]any {
	if modelName == "" {
		modelName = modelID
	}
	ev := mpChatEvent(conversationID, mpSchoolActivityID)
	ev["requestModelId"] = modelID
	ev["requestModelName"] = modelName
	return ev
}

// MiniPlaybookEvents builds the two-event miniprogram playbook sequence: the
// CTA click and the prompt send that follows it.  Both events share the case
// identity block, which the mp report shape does not otherwise carry.
func MiniPlaybookEvents(caseID, caseName string) []map[string]any {
	base := map[string]any{
		"id":           caseID,
		"name":         caseName,
		"type":         "document",
		"categoryId":   "",
		"categoryName": "",
		"skills":       "",
		"skillNames":   "",
	}
	cta := cloneEvent(base)
	cta["eventCode"] = "playbook_cta_click"
	cta["source"] = "discover"
	cta["position"] = 1
	cta["extVersion"] = miniExtVersion

	send := cloneEvent(base)
	send["eventCode"] = "playbook_prompt_send"
	send["source"] = "discover"
	send["promptLength"] = 96
	send["isOfficial"] = 1
	send["conversationId"] = "wb2api-mp-pb-" + clientToken()
	send["extVersion"] = miniExtVersion

	return []map[string]any{cta, send}
}

// cloneEvent copies an event map so the two elements of a sequence cannot alias
// each other's keys.
func cloneEvent(ev map[string]any) map[string]any {
	out := make(map[string]any, len(ev)+6)
	for k, v := range ev {
		out[k] = v
	}
	return out
}
