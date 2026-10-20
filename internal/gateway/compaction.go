package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Gateway-owned Responses compaction.
//
// OpenAI's own compaction item carries an opaque blob that only OpenAI can
// decrypt, and no third-party gateway can mint or read it.  This file implements
// the honest equivalent for this deployment: the same wire shape
// (`type: "compaction"`, `encrypted_content`), but the blob is a summary this
// gateway produced and signed, and only this gateway replays it.
//
// The blob is therefore never treated as opaque-forever state and never sent
// upstream as ciphertext.  On the way in, a blob that verifies is replaced by
// the assistant turn it stands for; a blob that does not verify (OpenAI's own,
// another deployment's, or a tampered one) is dropped exactly as foreign
// ciphertext always was.
// ---------------------------------------------------------------------------

// compactionBlobVersion prefixes every blob this gateway mints, so a future
// format change is detected rather than misread.
const compactionBlobVersion = "c2c1"

// compactionKeyFileMode is the mode the signing key is created with.  It is a
// secret only insofar as a forged blob would let a caller inject an assistant
// turn; the file never holds a vendor credential.
const compactionKeyFileMode = 0o600

// compactionPayload is the signed content of a blob.
type compactionPayload struct {
	// Version is repeated inside the signed body so a downgrade cannot be
	// smuggled past the prefix check.
	Version int `json:"v"`
	// Summary is the assistant turn the blob replays as.
	Summary string `json:"summary"`
	// SourceMessages is how many turns the summary folded, for diagnostics and
	// for the operator's usage row.
	SourceMessages int `json:"n,omitempty"`
	// Model is the model the summary was produced for.  It is advisory: a blob
	// may legitimately be replayed against a sibling model, so the reader does
	// not reject a mismatch.
	Model string `json:"model,omitempty"`
	// CreatedAt is a Unix second, kept for diagnostics and future expiry.
	CreatedAt int64 `json:"ts,omitempty"`
}

// compactionSigner mints and verifies compaction blobs.
//
// A nil *compactionSigner is usable and simply refuses to verify anything, which
// is the correct behaviour for an embedder that never configured a data
// directory: it can still produce a summary, it just cannot persist one across
// requests.
type compactionSigner struct {
	keyPath string

	mu  sync.Mutex
	key []byte
}

// newCompactionSigner builds a signer backed by the given key file.  An empty
// path selects an in-process key, which is the right behaviour when the gateway
// has no data directory.
func newCompactionSigner(keyPath string, warnf func(string, ...any)) *compactionSigner {
	s := &compactionSigner{keyPath: strings.TrimSpace(keyPath)}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadKeyLocked(); err != nil && warnf != nil {
		warnf("compaction: using an in-process signing key: %v", err)
	}
	return s
}

// loadKeyLocked reads the key, generating and persisting one on first use.
// Callers hold s.mu.
func (s *compactionSigner) loadKeyLocked() error {
	if s.keyPath != "" {
		if raw, err := os.ReadFile(s.keyPath); err == nil {
			if key, ok := decodeCompactionKey(raw); ok {
				s.key = key
				return nil
			}
			// A corrupt key file must not brick compaction: fall through to a
			// fresh in-process key rather than failing every request.
		} else if !errors.Is(err, os.ErrNotExist) {
			s.key = randomCompactionKey()
			return err
		}
	}
	s.key = randomCompactionKey()
	if s.keyPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.keyPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.keyPath, []byte(base64.RawURLEncoding.EncodeToString(s.key)), compactionKeyFileMode)
}

func randomCompactionKey() []byte {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; a fixed key would be worse than
		// useless, so fall back to a hash of the clock rather than a constant.
		sum := sha256.Sum256([]byte(fmt.Sprintf("client2api-compaction-%d", time.Now().UnixNano())))
		return sum[:]
	}
	return b[:]
}

func decodeCompactionKey(raw []byte) ([]byte, bool) {
	key, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(key) < 16 {
		return nil, false
	}
	return key, true
}

// mint returns a signed blob for payload.
func (s *compactionSigner) mint(summary string, meta compactionPayload) (string, error) {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return "", errors.New("compaction: refusing to mint an empty summary")
	}
	meta.Version = 1
	meta.Summary = summary
	if meta.CreatedAt == 0 {
		meta.CreatedAt = time.Now().Unix()
	}
	body, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(body)
	signature := s.sign(encoded)
	return compactionBlobVersion + "." + encoded + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// parse verifies a blob and returns the summary it carries.  ok is false for
// anything this deployment did not mint: a foreign ciphertext, a tampered blob,
// a wrong-version blob, or one whose key is gone.
func (s *compactionSigner) parse(blob string) (string, bool) {
	if s == nil {
		return "", false
	}
	blob = strings.TrimSpace(blob)
	if blob == "" {
		return "", false
	}
	parts := strings.Split(blob, ".")
	if len(parts) != 3 || parts[0] != compactionBlobVersion {
		return "", false
	}
	want := s.sign(parts[1])
	got, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(want, got) {
		return "", false
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	var payload compactionPayload
	if err := json.Unmarshal(body, &payload); err != nil || payload.Version != 1 {
		return "", false
	}
	summary := strings.TrimSpace(payload.Summary)
	if summary == "" {
		return "", false
	}
	return summary, true
}

// looksLikeCompactionBlob reports whether a string has this gateway's blob
// shape, independent of whether it verifies.  It exists so an unverifiable blob
// is dropped rather than mistaken for a plaintext summary by the legacy
// heuristic: our blobs are short and dotted, which that heuristic (which looks
// for long undelimited base64 runs) does not catch.
func looksLikeCompactionBlob(s string) bool {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, compactionBlobVersion+".") {
		return false
	}
	return len(strings.Split(s, ".")) == 3
}

// sign returns the HMAC of encoded.  The receiver may be nil, in which case the
// result never verifies (the caller's parse already rejects a nil receiver).
func (s *compactionSigner) sign(encoded string) []byte {
	s.mu.Lock()
	key := s.key
	s.mu.Unlock()
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(encoded))
	return mac.Sum(nil)
}

// ---------------------------------------------------------------------------
// Summarisation
// ---------------------------------------------------------------------------

// compactionSystemPrompt is the instruction the gateway hands the summariser.
// It is deliberately about state an agent needs to continue, not about prose: a
// generic "summarise this" loses the file names, decisions and pending steps that
// are the entire value of the history.
const compactionSystemPrompt = `You are compacting a long agent conversation so it can continue with less context.

Produce a dense, factual handover note. Preserve, in this order:
1. The user's goal and any explicit constraints or corrections.
2. What has already been done, with file paths, command names and exact identifiers.
3. Decisions made and the reason for each.
4. What is still pending, and the immediate next step.
5. Errors encountered that are still relevant, and how they were resolved.

Drop pleasantries, restatements and anything already superseded. Do not invent
facts. If something is unknown, say so. Write plain text, no headings.`

// compactionSummaryMaxChars bounds the summary the gateway accepts from the
// model.  A "summary" longer than this is not a summary, and carrying it forward
// would defeat the point of compacting.
const compactionSummaryMaxChars = 32 << 10

// compactionMaxRounds bounds how many upstream summarisation calls one
// compaction may make, so a pathological conversation cannot turn into a fan-out
// of requests.  Each round folds one prefix chunk.
const compactionMaxRounds = 4

// compactionInputBudget is how much of a chunk is sent to the summariser.  It is
// well below every vendor window this gateway serves so the summariser itself is
// never refused for size.
const compactionInputBudget = 32 * 1024

// compactConversation folds older turns of req into a single summary, keeping
// the newest turns intact, and returns the replacement message list plus what it
// did.
//
// The split is deliberate: the caller's most recent request must survive
// verbatim, because a summary is not a substitute for the question being asked.
// Older history, which is exactly what no longer fits, is what gets folded.
func compactConversation(ctx context.Context, s *server, client core.Client, req *core.ChatRequest, keepTurns int) (summary string, folded int, used *core.Usage, err error) {
	if req == nil || len(req.Messages) == 0 {
		return "", 0, nil, errors.New("compaction: nothing to compact")
	}
	if keepTurns <= 0 {
		keepTurns = 1
	}
	start, ok := compactionSplit(req.Messages, keepTurns)
	if !ok {
		// Too little history to fold: the newest turn is the whole request.
		return "", 0, nil, nil
	}
	older := req.Messages[:start]
	if len(older) == 0 {
		return "", 0, nil, nil
	}

	chunks := splitForSummarisation(older, compactionInputBudget)
	if len(chunks) > compactionMaxRounds {
		// Fold the oldest chunks together first so the round budget is not spent
		// on separate calls; the tail chunks are the ones worth summarising well.
		chunks = mergeOldest(chunks, len(chunks)-compactionMaxRounds+1)
	}

	var (
		parts    []string
		totalUse core.Usage
		haveUse  bool
	)
	for _, chunk := range chunks {
		text, use, cerr := summariseMessages(ctx, s, client, req.Model, chunk)
		if cerr != nil {
			return "", 0, nil, cerr
		}
		if text = strings.TrimSpace(text); text != "" {
			parts = append(parts, text)
		}
		if use != nil {
			totalUse.PromptTokens += use.PromptTokens
			totalUse.CompletionTokens += use.CompletionTokens
			totalUse.TotalTokens += use.TotalTokens
			totalUse.ReasoningTokens += use.ReasoningTokens
			totalUse.CachedTokens += use.CachedTokens
			haveUse = true
		}
	}
	summary = strings.Join(parts, "\n\n")
	if summary == "" {
		return "", 0, nil, errors.New("compaction: the summariser returned nothing")
	}
	if len(summary) > compactionSummaryMaxChars {
		summary = summary[:compactionSummaryMaxChars]
	}
	if haveUse {
		return summary, len(older), &totalUse, nil
	}
	return summary, len(older), nil, nil
}

// compactionSplit returns the index of the first message that must be kept
// verbatim, given how many trailing turns to preserve.  It never cuts between an
// assistant tool call and its results, so the retained tail is a valid history.
func compactionSplit(msgs []core.Message, keepTurns int) (int, bool) {
	if len(msgs) == 0 {
		return 0, false
	}
	// Walk back over the requested number of user-authored turns; a "turn" here
	// is a user message plus everything the assistant and tools produced for it.
	seen := 0
	idx := len(msgs)
	for idx > 0 {
		idx--
		if strings.EqualFold(strings.TrimSpace(msgs[idx].Role), "user") {
			seen++
			if seen >= keepTurns {
				break
			}
		}
	}
	if seen < keepTurns {
		// The whole history is fewer than keepTurns user turns: fold nothing, or
		// there would be no question left to answer.
		return 0, false
	}
	// Never begin the kept tail on a tool result: its assistant call would have
	// been folded away and vendors reject the orphan.
	for idx < len(msgs) && strings.EqualFold(strings.TrimSpace(msgs[idx].Role), "tool") {
		idx++
	}
	if idx <= 0 || idx >= len(msgs) {
		return 0, false
	}
	return idx, true
}

// splitForSummarisation groups messages into chunks whose estimated size stays
// under budget.  A single message larger than the budget becomes its own chunk
// trimmed to fit, because dropping it entirely would lose the fact that it
// happened.
func splitForSummarisation(msgs []core.Message, budget int) [][]core.Message {
	if budget <= 0 {
		budget = compactionInputBudget
	}
	var (
		out   [][]core.Message
		cur   []core.Message
		curSz int
	)
	flush := func() {
		if len(cur) > 0 {
			out = append(out, cur)
			cur = nil
			curSz = 0
		}
	}
	for _, m := range msgs {
		sz := estimateMessageTokens(m)
		if sz > budget {
			flush()
			out = append(out, []core.Message{truncateMessageForSummary(m, budget)})
			continue
		}
		if curSz+sz > budget {
			flush()
		}
		cur = append(cur, m)
		curSz += sz
	}
	flush()
	return out
}

// truncateMessageForSummary caps one oversized turn so it can be summarised at
// all.  The head and tail are both kept: the head usually names the file or
// command and the tail carries the outcome.
func truncateMessageForSummary(m core.Message, budget int) core.Message {
	maxChars := budget * 2
	if maxChars < 2048 {
		maxChars = 2048
	}
	if len(m.Content) > maxChars {
		half := maxChars / 2
		m.Content = m.Content[:half] + "\n…[truncated]…\n" + m.Content[len(m.Content)-half:]
	}
	if len(m.Reasoning) > maxChars {
		m.Reasoning = m.Reasoning[:maxChars]
	}
	return m
}

// mergeOldest folds the first n chunks into one so the round budget is respected.
func mergeOldest(chunks [][]core.Message, n int) [][]core.Message {
	if n <= 1 || len(chunks) <= 1 {
		return chunks
	}
	if n > len(chunks) {
		n = len(chunks)
	}
	merged := make([]core.Message, 0, 64)
	for _, c := range chunks[:n] {
		merged = append(merged, c...)
	}
	out := make([][]core.Message, 0, len(chunks)-n+1)
	out = append(out, merged)
	out = append(out, chunks[n:]...)
	return out
}

// summariseMessages runs one bounded summarisation call through the normal
// routing path, so account selection, rotation and accounting all behave the
// same way a user-visible call does.
func summariseMessages(ctx context.Context, s *server, client core.Client, model string, msgs []core.Message) (string, *core.Usage, error) {
	req := &core.ChatRequest{
		Model:    model,
		Messages: append([]core.Message{{Role: "system", Content: compactionSystemPrompt}}, msgs...),
		Stream:   true,
	}
	stream, err := s.openStream(ctx, client, req)
	if err != nil {
		return "", nil, err
	}
	defer stream.Close()

	var (
		text strings.Builder
		use  *core.Usage
	)
	for {
		ev, rerr := stream.Recv()
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return "", nil, rerr
		}
		switch ev.Type {
		case core.EventDelta:
			if ev.Delta != "" {
				text.WriteString(ev.Delta)
				if text.Len() > compactionSummaryMaxChars {
					// Enough is enough: stop paying for a runaway answer.
					return text.String()[:compactionSummaryMaxChars], use, nil
				}
			}
		case core.EventUsage:
			if ev.Usage != nil {
				use = ev.Usage
			}
		case core.EventDone:
			return text.String(), use, nil
		case core.EventError:
			if ev.Err != nil {
				return "", nil, ev.Err
			}
		}
	}
	return text.String(), use, nil
}
