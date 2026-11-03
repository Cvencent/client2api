package panel

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"client2api/internal/core"
)

// DefaultChatHistoryFileName is the relative path under the data directory.
// Conversation records are operator-visible state, so they live beside the
// other panel stores (balance_cache.json, checkin_state.json) rather than in
// the browser: a different browser, or the same browser after clearing site
// data, must still see every conversation.
const DefaultChatHistoryFileName = "panel/chat_history.json"

// maxChatTitleRunes bounds an operator-authored conversation title.  It follows
// maxAccountNoteRunes (configwrite.go) for the same two reasons, and it is the
// server-side half of issue #143: the title is drawn in the chat header, and an
// unbounded one let a single paste of 1,000,000 characters (measured: 200 OK,
// 1,002,152 bytes on disk, an 8,000,489px-wide header) paint over the client and
// model dropdowns -- so the operator could neither read the line nor click what
// was under it.  The same field is already ellipsised in the sidebar, so the
// header was the one place a long title was not a design choice.
//
// 120 runes is the note's number: enough for a pasted sentence in Chinese or a
// fairly long command line, small enough that the header truncation is about
// reading, not about surviving a megabyte.
const maxChatTitleRunes = 120

// clampChatTitle trims and caps a title, reporting whether it had to cut.  The
// caller rejects the write rather than storing a silently shortened string: the
// page shows the returned title, so a quiet cut would look like the save simply
// worked and leave the operator wondering where the rest went (the same rule
// #121/#122 settled for the queue labels).
func clampChatTitle(title string) (string, bool) {
	title = strings.TrimSpace(title)
	if title == "" {
		return "", false
	}
	if utf8.RuneCountInString(title) <= maxChatTitleRunes {
		return title, false
	}
	// Cut on a rune boundary, never mid-character: a byte slice here would
	// leave a broken UTF-8 tail that the JSON encoder replaces with U+FFFD.
	runes := []rune(title)
	return string(runes[:maxChatTitleRunes]), true
}

// ChatTurn is one persisted message.  Reasoning is kept apart from Content so
// the UI can show a collapsed "thinking" block without folding it into the
// answer it renders as the assistant's text.
type ChatTurn struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	Reasoning string `json:"reasoning,omitempty"`
	// Meta carries the per-turn diagnostics the test page shows (HTTP status,
	// latency, token counts). It is display-only and never fed back upstream.
	Meta string `json:"meta,omitempty"`
	At   int64  `json:"at"`
}

// ChatThread is one conversation: its own title, the model/account it was last
// used with, and the complete ordered transcript.
type ChatThread struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Client    string     `json:"client,omitempty"`
	Model     string     `json:"model,omitempty"`
	Account   string     `json:"account,omitempty"`
	Key       string     `json:"key,omitempty"`
	Messages  []ChatTurn `json:"messages"`
	CreatedAt int64      `json:"created_at"`
	UpdatedAt int64      `json:"updated_at"`
}

type chatHistoryFile struct {
	Version       int          `json:"version"`
	Conversations []ChatThread `json:"conversations"`
}

// chatHistoryStore is the process-wide transcript store.  It is deliberately
// small: the panel owns the data, no module sees it, and every mutation writes
// the whole document atomically so a crash cannot publish a half-written file.
type chatHistoryStore struct {
	mu        sync.Mutex
	persistMu sync.Mutex
	path      string
	now       func() time.Time
	// errs remembers the last disk failure so the overview can badge it and
	// the log ring can record it the moment it happens (issue #34).
	errs storeErrTracker
	// lastMs makes the timestamps strictly increasing.  Two creates inside one
	// millisecond would otherwise tie, and List's tie-break (by id) would show
	// the older conversation above the one the operator just touched.
	lastMs  int64
	threads map[string]ChatThread
}

// stamp returns a strictly increasing millisecond timestamp.
func (s *chatHistoryStore) stamp() int64 {
	ms := s.now().UnixMilli()
	if ms <= s.lastMs {
		ms = s.lastMs + 1
	}
	s.lastMs = ms
	return ms
}

func newChatHistoryStore(path string) *chatHistoryStore {
	s := &chatHistoryStore{
		path:    strings.TrimSpace(path),
		now:     time.Now,
		threads: map[string]ChatThread{},
	}
	s.load()
	return s
}

func (s *chatHistoryStore) load() {
	if s == nil || s.path == "" {
		return
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var doc chatHistoryFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		return
	}
	for _, t := range doc.Conversations {
		if t.ID == "" {
			continue
		}
		if t.Messages == nil {
			t.Messages = []ChatTurn{}
		}
		s.threads[t.ID] = t
	}
}

// ChatThreadSummary is one row of the conversation list: everything the sidebar
// renders, and nothing else.
//
// The list used to carry the full `messages` array of every thread ("so
// selecting a thread is one round trip"), which made a page that shows titles
// cost the size of the whole library: 340 threads measured 5.89 MB, re-sent on
// entry, on refresh, and after every completed turn (issue #127).  The
// transcript is already served per-thread by GET
// /panel/api/chat/conversations/<id>, so the list only needs the summary that
// the sidebar actually draws.  MessageCount is included because the sidebar
// shows it and the client would otherwise have to fetch to learn it.
type ChatThreadSummary struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Client    string `json:"client,omitempty"`
	Model     string `json:"model,omitempty"`
	Account   string `json:"account,omitempty"`
	Key       string `json:"key,omitempty"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	// MessageCount is len(messages) without shipping them.
	MessageCount int `json:"message_count"`
}

// ListSummaries returns every conversation without its transcript, newest
// update first (issue #127).
func (s *chatHistoryStore) ListSummaries() []ChatThreadSummary {
	if s == nil {
		return []ChatThreadSummary{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ChatThreadSummary, 0, len(s.threads))
	for _, t := range s.threads {
		out = append(out, ChatThreadSummary{
			ID:           t.ID,
			Title:        t.Title,
			Client:       t.Client,
			Model:        t.Model,
			Account:      t.Account,
			Key:          t.Key,
			CreatedAt:    t.CreatedAt,
			UpdatedAt:    t.UpdatedAt,
			MessageCount: len(t.Messages),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt != out[j].UpdatedAt {
			return out[i].UpdatedAt > out[j].UpdatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// List returns every conversation WITH its transcript, newest update first.
// Only the single-thread endpoint and the store's own callers need this; the
// list endpoint serves ListSummaries (issue #127).
func (s *chatHistoryStore) List() []ChatThread {
	if s == nil {
		return []ChatThread{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ChatThread, 0, len(s.threads))
	for _, t := range s.threads {
		out = append(out, copyThread(t))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt != out[j].UpdatedAt {
			return out[i].UpdatedAt > out[j].UpdatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (s *chatHistoryStore) Get(id string) (ChatThread, bool) {
	if s == nil {
		return ChatThread{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.threads[strings.TrimSpace(id)]
	if !ok {
		return ChatThread{}, false
	}
	return copyThread(t), true
}

// Create mints a thread.  An empty title becomes the same placeholder the page
// uses, so a stale client cannot create an unnamed row.
func (s *chatHistoryStore) Create(in ChatThread) (ChatThread, error) {
	if s == nil {
		return ChatThread{}, errors.New("no chat history store")
	}
	title, err := titleOrError(in.Title)
	if err != nil {
		return ChatThread{}, err
	}
	if title == "" {
		title = "新对话"
	}
	s.mu.Lock()
	now := s.stamp()
	t := ChatThread{
		ID:        newChatThreadID(),
		Title:     title,
		Client:    strings.TrimSpace(in.Client),
		Model:     strings.TrimSpace(in.Model),
		Account:   strings.TrimSpace(in.Account),
		Key:       strings.TrimSpace(in.Key),
		Messages:  []ChatTurn{},
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.threads[t.ID] = t
	s.mu.Unlock()
	s.persist()
	return copyThread(t), nil
}

// Update applies the mutable fields of a thread.  A nil slice means "leave the
// messages alone"; a non-nil one replaces the transcript wholesale, which is
// what the page does after a turn finishes.
func (s *chatHistoryStore) Update(id string, patch ChatThread) (ChatThread, error) {
	if s == nil {
		return ChatThread{}, errors.New("no chat history store")
	}
	id = strings.TrimSpace(id)
	s.mu.Lock()
	t, ok := s.threads[id]
	if !ok {
		s.mu.Unlock()
		return ChatThread{}, errChatThreadNotFound
	}
	if patch.Title != "" {
		title, err := titleOrError(patch.Title)
		if err != nil {
			s.mu.Unlock()
			return ChatThread{}, err
		}
		t.Title = title
	}
	if patch.Client != "" {
		t.Client = strings.TrimSpace(patch.Client)
	}
	if patch.Model != "" {
		t.Model = strings.TrimSpace(patch.Model)
	}
	if patch.Account != "" {
		t.Account = strings.TrimSpace(patch.Account)
	}
	if patch.Key != "" {
		t.Key = strings.TrimSpace(patch.Key)
	}
	if patch.Messages != nil {
		t.Messages = copyTurns(patch.Messages)
	}
	if t.Title == "" {
		t.Title = "新对话"
	}
	t.UpdatedAt = s.stamp()
	s.threads[id] = t
	s.mu.Unlock()
	s.persist()
	return copyThread(t), nil
}

// Append adds one turn.  The writer is the page itself, so a lost update is
// worse than a serialized write: this holds the lock across the read-modify
// part and only then persists.
func (s *chatHistoryStore) Append(id string, turn ChatTurn) (ChatThread, error) {
	if s == nil {
		return ChatThread{}, errors.New("no chat history store")
	}
	id = strings.TrimSpace(id)
	if turn.At == 0 {
		turn.At = s.stamp()
	}
	s.mu.Lock()
	t, ok := s.threads[id]
	if !ok {
		s.mu.Unlock()
		return ChatThread{}, errChatThreadNotFound
	}
	t.Messages = append(append([]ChatTurn(nil), t.Messages...), turn)
	t.UpdatedAt = s.stamp()
	s.threads[id] = t
	s.mu.Unlock()
	s.persist()
	return copyThread(t), nil
}

func (s *chatHistoryStore) Rename(id, title string) (ChatThread, error) {
	// Update is the one gate: it enforces the rune cap, so this path cannot
	// disagree with the PATCH route about what is storable (issue #143).
	title = strings.TrimSpace(title)
	if title == "" {
		return ChatThread{}, errors.New("title is required")
	}
	return s.Update(id, ChatThread{Title: title})
}

func (s *chatHistoryStore) Delete(id string) bool {
	if s == nil {
		return false
	}
	id = strings.TrimSpace(id)
	s.mu.Lock()
	if _, ok := s.threads[id]; !ok {
		s.mu.Unlock()
		return false
	}
	delete(s.threads, id)
	s.mu.Unlock()
	s.persist()
	return true
}

func (s *chatHistoryStore) Clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.threads = map[string]ChatThread{}
	s.mu.Unlock()
	s.persist()
}

func (s *chatHistoryStore) persist() {
	if s == nil || s.path == "" {
		return
	}
	s.persistMu.Lock()
	defer s.persistMu.Unlock()

	s.mu.Lock()
	doc := chatHistoryFile{Version: 1, Conversations: make([]ChatThread, 0, len(s.threads))}
	for _, t := range s.threads {
		doc.Conversations = append(doc.Conversations, copyThread(t))
	}
	s.mu.Unlock()
	sort.Slice(doc.Conversations, func(i, j int) bool {
		return doc.Conversations[i].ID < doc.Conversations[j].ID
	})

	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		s.errs.failed("chat history", err)
		return
	}
	if err := core.WriteFileAtomic(s.path, append(raw, '\n')); err != nil {
		s.errs.failed("chat history", err)
	}
}

// LastError exposes the most recent disk failure to the overview payload
// (issue #34); "" when every write has landed.
func (s *chatHistoryStore) LastError() string { return s.errs.LastError() }

var errChatThreadNotFound = errors.New("chat thread not found")

// chatTitleTooLongError is a rejected write, not a failure: the operator typed
// something the page should hand back rather than store (issue #143).
type chatTitleTooLongError struct{ n int }

func (e chatTitleTooLongError) Error() string {
	return fmt.Sprintf("对话标题最长 %d 个字符（收到 %d 个）", maxChatTitleRunes, e.n)
}

// titleOrError applies the cap.  A write over the limit is refused; the caller
// reports the limit so the page can tell the operator instead of pretending the
// save worked.
func titleOrError(raw string) (string, error) {
	title, cut := clampChatTitle(raw)
	if cut {
		return "", chatTitleTooLongError{n: utf8.RuneCountInString(strings.TrimSpace(raw))}
	}
	return title, nil
}

func copyTurns(in []ChatTurn) []ChatTurn {
	if in == nil {
		return []ChatTurn{}
	}
	return append([]ChatTurn(nil), in...)
}

func copyThread(in ChatThread) ChatThread {
	in.Messages = copyTurns(in.Messages)
	return in
}

func newChatThreadID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "chat-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return "chat-" + hex.EncodeToString(b[:])
}

// chatHistoryPath resolves the store file for this panel.  An empty DataDir
// keeps the store in memory, which is what tests and embedders without a data
// directory get.
func chatHistoryPath(dataDir string) string {
	if strings.TrimSpace(dataDir) == "" {
		return ""
	}
	return filepath.Join(dataDir, filepath.FromSlash(DefaultChatHistoryFileName))
}

func (p *panel) handleChatConversations(w http.ResponseWriter, r *http.Request) {
	if p == nil || p.chatHistory == nil {
		writeErr(w, http.StatusServiceUnavailable, "chat history is unavailable")
		return
	}
	switch r.Method {
	case http.MethodGet:
		// Summaries only, plus a conditional request: this endpoint is read on
		// every page entry, every refresh, and after every completed turn, and
		// used to cost the size of the whole library each time (issue #127).
		// Same shape as /panel/api/models (models.go): hash what the client can
		// see, honour If-None-Match with a bodiless 304.
		summaries := p.chatHistory.ListSummaries()
		body, err := json.Marshal(map[string]any{"conversations": summaries})
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "marshal conversations: "+err.Error())
			return
		}
		sum := sha256.Sum256(body)
		etag := `"` + hex.EncodeToString(sum[:16]) + `"`
		w.Header().Set("ETag", etag)
		if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		_, _ = w.Write([]byte("\n"))

	case http.MethodPost:
		var in ChatThread
		if !decodeJSON(w, r, &in) {
			return
		}
		thread, err := p.chatHistory.Create(in)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, thread)

	case http.MethodDelete:
		p.chatHistory.Clear()
		writeJSON(w, http.StatusOK, map[string]any{"cleared": true})

	default:
		writeErr(w, http.StatusMethodNotAllowed, "use GET, POST or DELETE")
	}
}

func (p *panel) handleChatConversation(w http.ResponseWriter, r *http.Request, id string) {
	if p == nil || p.chatHistory == nil {
		writeErr(w, http.StatusServiceUnavailable, "chat history is unavailable")
		return
	}
	id = strings.TrimSpace(id)
	if id == "" {
		writeErr(w, http.StatusBadRequest, "conversation id is required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		thread, ok := p.chatHistory.Get(id)
		if !ok {
			writeErr(w, http.StatusNotFound, "conversation not found: "+id)
			return
		}
		writeJSON(w, http.StatusOK, thread)

	case http.MethodPatch:
		var patch ChatThread
		if !decodeJSON(w, r, &patch) {
			return
		}
		thread, err := p.chatHistory.Update(id, patch)
		if errors.Is(err, errChatThreadNotFound) {
			writeErr(w, http.StatusNotFound, "conversation not found: "+id)
			return
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, thread)

	case http.MethodDelete:
		if !p.chatHistory.Delete(id) {
			writeErr(w, http.StatusNotFound, "conversation not found: "+id)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": id})

	default:
		writeErr(w, http.StatusMethodNotAllowed, "use GET, PATCH or DELETE")
	}
}

// handleChatConversationRoute extracts the id from the collection's subpath and
// dispatches to the single-conversation handler.
func (p *panel) handleChatConversationRoute(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/panel/api/chat/conversations/")
	p.handleChatConversation(w, r, id)
}

// initChatHistory wires the persisted conversation store.  A panel with no
// data directory still gets an in-memory store, so tests and embedders keep
// working and the page never sees a nil store.
func (p *panel) initChatHistory() {
	if p == nil {
		return
	}
	p.chatHistory = newChatHistoryStore(chatHistoryPath(p.opts.DataDir))
}
