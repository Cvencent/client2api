package panel

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"client2api/internal/core"
)

// conversationRequest is the body of POST <base>/conversations and
// POST <base>/conversations/unbind.
type conversationRequest struct {
	// Key is the stickiness key.  Empty on a bind means "mint one", so the
	// browser never has to invent an identifier and two tabs cannot collide.
	Key string `json:"key"`
	// Account is the account id to pin.  Required on a bind.
	Account string `json:"account"`
	// Model is the model the caller is about to request.  Optional; it only
	// sharpens the read-back for a module whose health is model-scoped.
	Model string `json:"model"`
}

// conversations implements the panel's conversation-stickiness surface:
//
//	GET  <base>/conversations?key=…&model=…   read one binding
//	POST <base>/conversations                 {"account":"…","key":"…"}
//
// It exists so the chat tab can test ONE named account.  core.ChatRequest has
// no account field and the gateway must not guess the serving account (it
// learns it only from an error), so the only honest way to aim a request at a
// chosen credential is the module's own stickiness table: bind a fresh key to
// the account, then send the chat with options.conversation_id set to that key.
//
// Three refusals are deliberate:
//
//   - 501 when the module implements no core.ConversationBinder.  Seven of the
//     fourteen modules have no stickiness table at all; for them a per-account test
//     is impossible, and saying so beats a dropdown that silently tests
//     whatever the pool picked.
//   - 404 when the module does not know the account id.  A binding to an
//     unknown id is dropped at chat time by Resolve's usability check, so
//     accepting it would let the operator believe the chosen account answered
//     when a different one did.
//   - 400 when the key is missing on an unbind, or the body will not parse.
func (p *panel) conversations(w http.ResponseWriter, r *http.Request, c core.Client) {
	binder, ok := core.AsConversationBinder(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" has no conversation table to pin; it always serves from its pool")
		return
	}
	switch r.Method {
	case http.MethodGet:
		key := strings.TrimSpace(r.URL.Query().Get("key"))
		if key == "" {
			writeErr(w, http.StatusBadRequest, "key is required")
			return
		}
		id, bound := binder.ConversationAccount(key, strings.TrimSpace(r.URL.Query().Get("model")))
		writeJSON(w, http.StatusOK, map[string]any{"key": key, "account": id, "bound": bound})

	case http.MethodPost:
		var in conversationRequest
		if !decodeJSON(w, r, &in) {
			return
		}
		account := strings.TrimSpace(in.Account)
		if account == "" {
			writeErr(w, http.StatusBadRequest, "account is required")
			return
		}
		ctx, cancel := p.ctx(r, 10*time.Second)
		defer cancel()
		if !p.accountExists(ctx, c, account) {
			writeErr(w, http.StatusNotFound, "account not found: "+account)
			return
		}
		key := strings.TrimSpace(in.Key)
		if key == "" {
			key = newConversationKey()
		}
		binder.BindConversation(key, account)
		// Read back through the module rather than echoing what was asked for:
		// a binding the module will not honour (a parked credential, a model
		// the account cannot serve) must show up as bound:false now, not as a
		// surprise at chat time.
		id, bound := binder.ConversationAccount(key, strings.TrimSpace(in.Model))
		writeJSON(w, http.StatusOK, map[string]any{
			"key": key, "account": id, "bound": bound, "requested": account,
		})

	default:
		writeErr(w, http.StatusMethodNotAllowed, "use GET or POST")
	}
}

// unbindConversation implements POST <base>/conversations/unbind.  It is the
// administrative half: the chat tab drops the key when the operator stops
// testing an account, so the stickiness table does not fill with dead keys.
func (p *panel) unbindConversation(w http.ResponseWriter, r *http.Request, c core.Client) {
	binder, ok := core.AsConversationBinder(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" has no conversation table to pin; it always serves from its pool")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var in conversationRequest
	if !decodeJSON(w, r, &in) {
		return
	}
	key := strings.TrimSpace(in.Key)
	if key == "" {
		writeErr(w, http.StatusBadRequest, "key is required")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "forgotten": binder.UnbindConversation(key)})
}

// newConversationKey mints a stickiness key the operator never has to think
// about.  It deliberately does not name the account: the same account may be
// pinned under several keys at once, and a key derived from the account id would
// make two browser tabs overwrite each other's binding.
func newConversationKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "panel-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return "panel-" + hex.EncodeToString(b[:])
}
