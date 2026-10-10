package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"client2api/internal/alerts"
	"client2api/internal/core"
	"client2api/internal/hint"
	"client2api/internal/livecfg"
	"client2api/internal/modelmeta"
)

const maxBodyBytes = 64 << 20 // 64 MiB, generous for long agent transcripts

// DefaultService is the value of the X-Service response header.  The
// reference pins its own product name there so that a shared reverse proxy or
// an operator's log grep can tell which gateway answered.
const DefaultService = "client2api"

// Options configures the OpenAI-compatible gateway.
type Options struct {
	Addr string
	// APIKey is the inbound bearer used when Live is nil.  Empty disables
	// inbound auth.
	APIKey string
	// Live, when non-nil, supersedes APIKey and makes the few hot settings
	// editable without a restart.
	Live     *livecfg.Holder
	Registry *core.Registry
	Version  string
	Logger   *log.Logger
	// Service is the X-Service header value; empty means DefaultService.
	Service string
	// Guard carries the process-level egress-IP WAF gate and the
	// degraded-prompt window.  Nil disables both behaviours.
	Guard *core.Guard
	// Panel, when non-nil, is mounted at /panel/ and as the catch-all.
	Panel http.Handler
	// Stats and Usage are the gateway's instrumentation.  They are created by
	// the caller (see cmd/client2api) so that the panel can read the very same
	// numbers the gateway serves on /v1/status.  Nil selects private ones,
	// which then only the gateway can see.
	Stats *Stats
	Usage *UsageStore
	// NotifyAlert receives operator-facing platform health alerts.  It is
	// called synchronously and may be nil.
	// ModelOverrides is the operator-edited model metadata shared with the panel.
	ModelOverrides *modelmeta.OverrideStore
	NotifyAlert    func(alerts.Alert)
}

type server struct {
	opts    Options
	started time.Time
	stats   *Stats

	// limMu guards limiters, the per-platform in-flight brakes installed by
	// the platform configuration page.  See platform_limit.go.
	limMu    sync.Mutex
	limiters map[string]*platformLimiter
	// accountLimiters is the second brake, keyed by (platform, account): the
	// operator's per-account ceiling.  limMu also guards this map.
	accountLimiters map[accountKey]*platformLimiter
	// sessionPlatforms keeps a bare-model conversation on the platform that
	// last served it, so failover does not ping-pong between a healthy and a
	// broken sibling on every turn.
	sessionPlatforms *sessionPlatforms
}

// NewServer builds the gateway's http.Server.  It is deliberately transport
// only: every vendor-specific decision lives behind core.Client.
func NewServer(opts Options) *http.Server {
	if opts.Stats == nil {
		opts.Stats = NewStats()
	}
	if opts.Usage == nil {
		opts.Usage = NewUsageStore(0)
	}
	if opts.Service == "" {
		opts.Service = DefaultService
	}
	s := &server{
		opts:             opts,
		started:          time.Now(),
		stats:            opts.Stats,
		sessionPlatforms: newSessionPlatforms(),
	}
	if s.opts.Logger == nil {
		s.opts.Logger = log.Default()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/models", s.auth(s.handleModels))
	mux.HandleFunc("/v1/chat/completions", s.auth(s.handleChat))
	mux.HandleFunc("/v1/responses", s.auth(s.handleResponses))
	mux.HandleFunc("/v1/status", s.auth(s.handleStatus))
	mux.HandleFunc("/v1/", s.auth(s.handleNotFound))
	if opts.Panel != nil {
		mux.Handle("/panel/", opts.Panel)
		mux.Handle("/", opts.Panel)
	} else {
		mux.HandleFunc("/", s.handleNotFound)
	}

	// The timeouts mirror the reference server (cmd/server/main.go:33):
	// ReadHeaderTimeout 30s, ReadTimeout 60s, IdleTimeout 120s.  ReadTimeout
	// bounds how long a client may dribble a request body in; there is
	// deliberately NO WriteTimeout, because a chat completion is streamed for
	// as long as the vendor keeps talking and a write deadline would cut it off.
	return &http.Server{
		Addr:              opts.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// ---------------------------------------------------------------------------
// live configuration
// ---------------------------------------------------------------------------

// apiKey returns the bearer currently in force.  A live holder wins over the
// startup value so that rotating the key does not need a restart; an empty
// key means auth is off, exactly as at startup.
func (s *server) apiKey() string {
	if s.opts.Live != nil {
		return s.opts.Live.Load().APIKey
	}
	return s.opts.APIKey
}

// maxRotate is how many extra accounts one inbound request may try.
func (s *server) maxRotate() int {
	n := core.MaxRotate
	if s.opts.Live != nil {
		if v := s.opts.Live.Load().MaxRotate; v > 0 {
			n = v
		}
	}
	if n < 0 {
		return 0
	}
	return n
}

// rotateBackoff is the wait before retry n, honouring a live base.
func (s *server) rotateBackoff(n int) time.Duration {
	base := core.RotateBackoffBase
	if s.opts.Live != nil {
		if v := s.opts.Live.Load().RotateBackoffBase; v > 0 {
			base = v
		}
	}
	return core.BackoffFrom(base, n)
}

// ---------------------------------------------------------------------------
// middleware
// ---------------------------------------------------------------------------

// auth enforces the shared bearer on every management and OpenAI surface.
//
// The comparison is deliberately the same shape as the reference's:
// ConstantTimeCompare over SHA-256 digests, so neither the length nor a
// common prefix of the configured key leaks through timing.  An empty key
// disables auth entirely (a documented single-operator deployment mode).
func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if key := s.apiKey(); key != "" {
			if !core.VerifyBearer(r.Header.Get("Authorization"), r.Header.Get("x-api-key"), key) {
				writeError(w, http.StatusUnauthorized, "invalid_request_error", "invalid API key")
				return
			}
		}
		next(w, r)
	}
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

// handleHealth follows the reference's load-balancer contract: 200 only while
// at least one registered client says it can serve, 503 otherwise.  A client
// that has not implemented core.HealthProvider counts as servable, so the
// endpoint degrades to "process is up" for modules that have not opted in.
func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	servable, clients, realms := s.health()
	status := http.StatusOK
	state := "ok"
	if !servable {
		status = http.StatusServiceUnavailable
		state = "unavailable"
	}
	w.Header().Set("X-Service", s.opts.Service)
	body := map[string]any{
		"status":  state,
		"service": s.opts.Service,
		"version": s.opts.Version,
		"uptime":  time.Since(s.started).Round(time.Second).String(),
		"clients": clients,
	}
	if len(realms) > 0 {
		body["realm_servable"] = realms
	}
	writeJSON(w, status, body)
}

// health aggregates every client's own verdict.  clients is the list of
// registered names whose module can currently serve (the historical, cheerful
// meaning of the field); realms merges the per-realm answers.
func (s *server) health() (bool, []string, map[string]bool) {
	all := s.opts.Registry.All()
	names := make([]string, 0, len(all))
	realms := map[string]bool{}
	servable := false
	for _, c := range all {
		h, ok := core.HealthOf(c)
		if !ok || h.Servable {
			servable = true
			names = append(names, c.Name())
			continue
		}
		for realm, up := range h.Realms {
			realms[realm] = realms[realm] || up
		}
	}
	return servable, names, realms
}

func (s *server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "invalid_request_error", "no such endpoint: "+r.URL.Path)
}

// autoModelPick is the display spelling chosen for one Auto/<model> entry.
// raw records the vendor id so a platform with an already-bare id wins over
// one whose id carries a vendor namespace.
type autoModelPick struct {
	display string
	raw     string
}

// betterAutoDisplay keeps the Auto/ catalogue stable regardless of the
// registration order of platforms.  A bare upstream id is the clearest
// spelling; otherwise the shorter, lexicographically earlier one wins.
func betterAutoDisplay(next, old autoModelPick) bool {
	nextBare := strings.EqualFold(next.raw, next.display)
	oldBare := strings.EqualFold(old.raw, old.display)
	if nextBare != oldBare {
		return nextBare
	}
	if len(next.display) != len(old.display) {
		return len(next.display) < len(old.display)
	}
	return next.display < old.display
}

func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	out := modelList{Object: "list", Data: []modelEntry{}}
	created := s.started.Unix()
	// Upstream catalogues are not guaranteed unique: the live Trae catalogue
	// really does return the same id twice, so de-duplicate before serving.
	seen := make(map[string]struct{}, 64)
	// resolved caches each advertised id's context/output so the alias table
	// below can reuse the target's numbers instead of re-resolving them.
	resolved := make(map[string]modelmeta.Meta, 64)
	// Auto/<model> is a gateway-level virtual model, not an upstream id.
	// Aggregate vendor catalogues under one stable display name so a caller
	// can ask the gateway to choose the platform.  Keep every platform-
	// qualified id below untouched: Auto/ is additive, never a replacement.
	auto := map[string]autoModelPick{} // canonical routing key -> display name
	for _, c := range s.opts.Registry.All() {
		models, err := c.Models(ctx)
		if err != nil {
			s.opts.Logger.Printf("models: client %s: %v", c.Name(), err)
			continue
		}
		for _, m := range models {
			id := c.Name() + "/" + m.ID
			if _, dup := seen[id]; dup {
				continue
			}
			// Routing refuses a blacklisted model for this platform, so
			// advertising it here would only lure a caller into a guaranteed
			// refusal.  The catalogue and the router must agree.
			if !s.opts.Registry.ModelAllowed(c.Name(), m.ID) {
				continue
			}
			if key := core.CanonicalModelID(m.ID); key != "" {
				pick := autoModelPick{display: core.ModelDisplayID(m.ID), raw: m.ID}
				if pick.display == "" {
					pick.display = m.ID
				}
				if old, ok := auto[key]; !ok || betterAutoDisplay(pick, old) {
					auto[key] = pick
				}
			}
			seen[id] = struct{}{}
			owned := m.OwnedBy
			if owned == "" {
				owned = c.Name()
			}
			// Resolve the window/output from override > vendor > official preset so
			// a caller can size its context even when the vendor reported nothing.
			meta := s.resolvedModelMeta(c, m.ID, m.Extra)
			resolved[id] = meta
			// Mirror the resolved numbers into Extra as well: some clients read the
			// OpenAI top-level field, others look inside extra. Fill only what the
			// vendor left empty so the vendor's own numbers are never overwritten.
			// Clone before adding: the map belongs to the module's catalogue and
			// mutating it here would leak resolved values back into the module.
			var extra map[string]any
			if len(m.Extra) > 0 {
				extra = make(map[string]any, len(m.Extra)+2)
				for k, v := range m.Extra {
					extra[k] = v
				}
			}
			if meta.ContextLength > 0 || meta.MaxOutputTokens > 0 {
				if extra == nil {
					extra = map[string]any{}
				}
				if _, ok := extra["context_length"]; !ok && meta.ContextLength > 0 {
					extra["context_length"] = meta.ContextLength
				}
				if _, ok := extra["max_output_tokens"]; !ok && meta.MaxOutputTokens > 0 {
					extra["max_output_tokens"] = meta.MaxOutputTokens
				}
			}
			out.Data = append(out.Data, modelEntry{
				ID:              id,
				Object:          "model",
				Created:         created,
				OwnedBy:         owned,
				ContextLength:   meta.ContextLength,
				MaxOutputTokens: meta.MaxOutputTokens,
				Extra:           extra,
			})
		}
	}
	sort.Slice(out.Data, func(i, j int) bool { return out.Data[i].ID < out.Data[j].ID })

	for key, display := range auto {
		id := "Auto/" + display.display
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out.Data = append(out.Data, modelEntry{
			ID:      id,
			Object:  "model",
			Created: created,
			OwnedBy: "auto",
			Extra:   map[string]any{"target": key},
		})
	}

	// Operator-declared equivalence groups are virtual models: a caller can
	// discover the group name and the concrete platform/model members behind
	// it without pretending the group is an upstream id.
	for name, group := range s.opts.Registry.ModelGroups() {
		if _, dup := seen[name]; dup {
			continue
		}
		members := make([]string, 0, len(group.Members))
		for _, member := range group.Members {
			members = append(members, member.Client+"/"+member.Model)
		}
		seen[name] = struct{}{}
		out.Data = append(out.Data, modelEntry{
			ID:      name,
			Object:  "model",
			Created: created,
			OwnedBy: "group",
			Extra:   map[string]any{"members": members},
		})
	}

	// The alias table is part of what this gateway serves, so it belongs in the
	// catalogue.  Without this a caller that lists models and then requests the
	// one it picked is fine, but a caller that reads the list cannot discover
	// the aliases at all -- "glm-5.3" resolves and is absent from /v1/models.
	// owned_by carries the alias name so a UI can tell an alias apart from a
	// real upstream id, and extra.target says what it points at.
	for alias, target := range s.opts.Registry.Aliases() {
		if _, dup := seen[alias]; dup {
			continue
		}
		// An alias is only useful while its target resolves.  Hiding one
		// that points at a disabled model keeps the list honest.
		if tc, tm, ok := strings.Cut(target, "/"); ok && tc != "" && tm != "" {
			if !s.opts.Registry.ModelAllowed(tc, tm) {
				continue
			}
		}
		seen[alias] = struct{}{}
		meta := resolved[target]
		aliasExtra := map[string]any{"target": target}
		if meta.ContextLength > 0 {
			aliasExtra["context_length"] = meta.ContextLength
		}
		if meta.MaxOutputTokens > 0 {
			aliasExtra["max_output_tokens"] = meta.MaxOutputTokens
		}
		out.Data = append(out.Data, modelEntry{
			ID:              alias,
			Object:          "model",
			Created:         created,
			OwnedBy:         "alias",
			ContextLength:   meta.ContextLength,
			MaxOutputTokens: meta.MaxOutputTokens,
			Extra:           aliasExtra,
		})
	}

	sort.Slice(out.Data, func(i, j int) bool { return out.Data[i].ID < out.Data[j].ID })
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	clients := make([]core.Status, 0, len(s.opts.Registry.All()))
	health := map[string]core.Health{}
	for _, c := range s.opts.Registry.All() {
		clients = append(clients, core.RedactStatus(c.Status(ctx)))
		if h, ok := core.HealthOf(c); ok {
			health[c.Name()] = h
		}
	}
	servable, active, realms := s.health()

	body := map[string]any{
		"version":  s.opts.Version,
		"service":  s.opts.Service,
		"uptime":   time.Since(s.started).Round(time.Second).String(),
		"requests": s.stats.Requests(),
		"failures": s.stats.Failures(),
		"clients":  clients,
		"servable": servable,
		"active":   active,
		"rotation": map[string]any{
			"max_rotate":          s.maxRotate(),
			"rotate_backoff_base": s.rotateBackoff(0).String(),
		},
	}
	if len(realms) > 0 {
		body["realm_servable"] = realms
	}
	if len(health) > 0 {
		body["client_health"] = health
	}
	if s.opts.Guard != nil {
		body["guard"] = s.opts.Guard.Snapshot()
	}
	if s.opts.Live != nil {
		sn := s.opts.Live.Load()
		body["live"] = map[string]any{
			"api_key_set":           sn.AuthEnabled(),
			"sanitize_fingerprints": sn.SanitizeFingerprints,
			"prompt_mode":           sn.PromptMode,
			"soft_cooldown":         sn.SoftCooldown.String(),
		}
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "use POST")
		return
	}
	s.stats.addRequest()

	// Exactly one usage record per accepted request, whatever the outcome.
	// The deferred call is the single place that stores it, so every early
	// return below -- unreadable body, invalid JSON, unknown model, upstream
	// refusal -- is counted as a failure with whatever tokens were reported.
	rec := UsageRecord{At: time.Now(), StartedAt: time.Now()}
	defer func() {
		rec.At = time.Now()
		s.opts.Usage.Record(rec)
	}()

	// The console row for this request.  It is created before the body is read
	// so that even a rejected request is visible, and the deferred done()
	// writes it exactly once on the way out.
	stat := newChatStat(rec.StartedAt, "", false)
	defer stat.done()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		stat.status = http.StatusBadRequest
		s.fail(&rec)
		writeError(w, http.StatusBadRequest, "invalid_request_error", "reading body: "+err.Error())
		return
	}

	var wire chatRequest
	if err := json.Unmarshal(body, &wire); err != nil {
		stat.status = http.StatusBadRequest
		s.fail(&rec)
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}
	rec.Model = wire.Model
	stat.model = wire.Model
	stat.mode = chatMode(wire.Stream)

	req, err := toCoreRequest(&wire)
	if err != nil {
		stat.status = http.StatusBadRequest
		s.fail(&rec)
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	// A caller that runs its own conversation turns can hand the turn id over;
	// a module that aggregates its upstream calls per turn honours it verbatim
	// instead of deriving one.
	req.ConversationRequestID = strings.TrimSpace(r.Header.Get("X-Conversation-Request-ID"))
	// The usage journal answers "which session made this call", so it records
	// the caller's own session identifier, exactly the one the router pins a
	// conversation to.  It is set here, once, so failover attempts below can
	// copy it onto their own rows without re-deriving it.
	rec.SessionID = sessionIDFor(req)
	// The thinking level belongs to the call, not to the attempt, so it is
	// resolved once here and copied onto every failover row below.
	rec.ReasoningEffort = reasoningEffortFor(&wire)

	s.routeAndServe(w, r, routeInput{
		requested:    wire.Model,
		req:          req,
		stream:       wire.Stream,
		modelForWire: wire.Model,
		rec:          &rec,
		stat:         stat,
		emitter: chatWireEmitter{
			includeUsage: wire.StreamOptions != nil && wire.StreamOptions.IncludeUsage,
		},
	})
}

// serveRemainingCandidates tries the rest of a bare-model candidate list.  It
// is called from routeAndServe once the first candidate has been refused in a
// way that a sibling platform could plausibly serve, and it speaks only in the
// protocol-neutral routeInput so that both endpoints fail over identically.
func (s *server) serveRemainingCandidates(w http.ResponseWriter, r *http.Request, in routeInput, candidates []core.Candidate, firstCandidate int, maxTokensExplicit bool) (core.Client, func() core.HintContext, error, bool) {
	rec, stat, baseReq := in.rec, in.stat, in.req

	var (
		lastClient core.Client
		lastHint   func() core.HintContext
		lastErr    error
	)
	for _, candidate := range candidates {
		client, upstreamModel := candidate.Client, candidate.Model
		rec.Candidate = firstCandidate
		firstCandidate++
		attemptReq := cloneChatRequest(baseReq)
		attemptReq.Model = upstreamModel
		if !maxTokensExplicit {
			attemptReq.MaxTokens = nil
			if n, ok := s.resolveMaxOutputTokens(r.Context(), client, upstreamModel); ok {
				attemptReq.MaxTokens = &n
			}
		}
		rec.Client = client.Name()
		rec.Model = client.Name() + "/" + upstreamModel
		rec.Realm = ""
		if h, ok := core.HealthOf(client); ok && len(h.Realms) == 1 {
			for realm := range h.Realms {
				rec.Realm = realm
			}
		}
		stat.model = rec.Model

		hintCtx := s.hintContextFunc(r.Context(), client, upstreamModel, attemptReq.Messages)
		var servedBy string
		attemptReq.ServedBy = &servedBy
		stream, err := s.openStream(r.Context(), client, attemptReq)
		if err == nil {
			s.opts.Registry.NoteModelSuccess(client.Name(), upstreamModel, time.Now())
			s.sessionPlatforms.bind(core.ConversationKeyOf(baseReq), client.Name())
			if servedBy != "" {
				rec.Account = servedBy
				stat.uid = servedBy
			}
			defer stream.Close()
			// The wire model is the caller's own name; only the module sees the
			// resolved upstream id, which already lives on rec.Model.
			a := emitArgs{s: s, client: client, model: in.modelForWire, stream: stream, hintCtx: hintCtx, rec: rec, stat: stat}
			if in.stream {
				in.emitter.emitStream(w, r, a)
			} else {
				in.emitter.emitBuffered(w, r, a)
			}
			return client, hintCtx, nil, true
		}

		// A candidate that cannot serve right now is demoted briefly, so the
		// next request does not pay for it again before trying a healthy
		// platform.  It stays in the list and is retried once the window
		// expires.
		if candidateUnavailable(err) {
			s.opts.Registry.NoteModelUnavailable(client.Name(), upstreamModel, time.Now())
		}

		rec.Account = core.ErrorAccountID(err)
		if rec.Account == "" {
			rec.Account = servedBy
		}
		stat.uid = rec.Account
		// Record the failed candidate in the recent list without counting it as
		// another inbound request.  The alert is evidence this happened; the
		// usage page must show the same evidence.
		s.opts.Usage.RecordAttempt(UsageRecord{
			At:              time.Now(),
			StartedAt:       rec.StartedAt,
			Client:          client.Name(),
			Realm:           rec.Realm,
			Account:         rec.Account,
			SessionID:       rec.SessionID,
			ReasoningEffort: rec.ReasoningEffort,
			Model:           client.Name() + "/" + upstreamModel,
			Candidate:       rec.Candidate,
			Failed:          true,
			Attempt:         true,
		})
		lastClient, lastHint, lastErr = client, hintCtx, err

		f, classified := core.AsFailure(err)
		retryable := classified && core.Retryable(f.Kind)
		if retryable {
			ev := s.opts.Registry.NoteModelFailure(client.Name(), upstreamModel, time.Now())
			s.publishPlatformAlert(client.Name(), upstreamModel, rec.Account, ev)
		}
		skippable := candidateUnavailable(err) || (!explicitPlatformRequest(in.requested, client.Name()) && modelRefusal(err))
		if (!retryable && !skippable) || (s.opts.Guard != nil && s.opts.Guard.IP.Active()) {
			return lastClient, lastHint, lastErr, false
		}
		s.opts.Logger.Printf("chat: platform %s model %s failed (%v), trying the next platform", client.Name(), upstreamModel, err)
	}
	return lastClient, lastHint, lastErr, false
}

// candidateUnavailable reports whether a candidate can be skipped without
// treating its refusal as an upstream answer.  The router may still have a
// healthy platform behind this one.
//
// modelRefusal reports whether the upstream answered that this model id does
// not exist on this platform.  A bare-name/Auto request did not choose the
// platform, so the router may move on to a candidate that does serve it; the
// account is not at fault, so nothing is parked.
func modelRefusal(err error) bool {
	f, ok := core.AsFailure(err)
	return ok && f.Kind == core.FailureOther && f.Status == http.StatusNotFound
}

// explicitPlatformRequest reports whether the caller named this platform in
// the model string ("cline/<id>").  An explicit choice is answered honestly:
// the gateway must not hide the refusal behind a different platform.
func explicitPlatformRequest(model, client string) bool {
	prefix, rest, ok := strings.Cut(strings.TrimSpace(model), "/")
	return ok && rest != "" && strings.EqualFold(prefix, client)
}

// ErrPlatformExhausted is the module's explicit statement that it has already
// walked its own account pool.  Re-entering that platform would only repeat the
// same exhausted rotation, so the gateway moves on.
func candidateUnavailable(err error) bool {
	if errors.Is(err, core.ErrPlatformExhausted) {
		return true
	}
	return errors.Is(err, core.ErrNotConfigured) || errors.Is(err, core.ErrBusy)
}

func cloneChatRequest(src *core.ChatRequest) *core.ChatRequest {
	if src == nil {
		return nil
	}
	cp := *src
	cp.Messages = append([]core.Message(nil), src.Messages...)
	for i := range cp.Messages {
		cp.Messages[i].Parts = append([]core.ContentPart(nil), src.Messages[i].Parts...)
		cp.Messages[i].ToolCalls = append([]core.ToolCall(nil), src.Messages[i].ToolCalls...)
	}
	cp.Tools = append([]core.Tool(nil), src.Tools...)
	cp.ToolChoice = append(json.RawMessage(nil), src.ToolChoice...)
	cp.Stop = append([]string(nil), src.Stop...)
	if src.Options != nil {
		cp.Options = make(map[string]any, len(src.Options))
		for k, v := range src.Options {
			cp.Options[k] = v
		}
	}
	cp.ServedBy = nil
	// A failover attempt gets its own per-account hook: the copy must not
	// inherit the original's acquirer or share its lease.
	cp.ClearAccountHook()
	return &cp
}

func (s *server) publishPlatformAlert(client, model, account string, ev core.HealthEvent) {
	if !ev.Alert {
		return
	}
	s.opts.Logger.Printf("chat: platform %s model %s failed repeatedly; cooling down for %s", client, model, core.PlatformCooldown)
	if s.opts.NotifyAlert != nil {
		s.opts.NotifyAlert(alerts.Alert{
			At:      time.Now(),
			Kind:    "platform_unhealthy",
			Client:  client,
			Model:   model,
			Account: account,
			Detail:  "three failed rounds in ten minutes; platform temporarily demoted",
			Count:   ev.Failures,
		})
	}
}

// openStream calls the module and, when the module classified the refusal as
// retryable, rotates to another account and tries again.
//
// The rotation lives here rather than in each module because it is policy, not
// protocol: the modules own which account to try next (their pool advances on
// each call) while the gateway owns how many times and how long to wait.  That
// split is what keeps a module free of any knowledge of the others.
//
// Three reference behaviours are reproduced deliberately:
//
//   - a WAF block is attributed to the egress IP, so the IP gate is fed on
//     every one of them and an activated gate stops the loop immediately
//     instead of multiplying one client request into MaxRotate refused ones;
//   - a content-policy refusal is *not* rotated (the content is the problem);
//     it triggers the degraded-prompt window and, when a Degrade hook is
//     installed, retries exactly once with the rewritten request;
//   - an unclassified error is never retried, because the gateway cannot know
//     whether a second account would help.
func (s *server) openStream(ctx context.Context, client core.Client, req *core.ChatRequest) (core.Stream, error) {
	guard := s.opts.Guard
	maxRotate := s.maxRotate()
	degraded := false
	var lastErr error

	for attempt := 0; ; attempt++ {
		if attempt > 0 && guard != nil && guard.IP.Active() {
			// The egress IP is already being refused; another account cannot
			// change that, so fail now and let the window expire.
			return nil, lastErr
		}

		// One slot per attempt, held for the life of the returned stream.  A
		// saturated platform is backpressure, not an account failure, so it is
		// returned immediately instead of being rotated.
		release, lerr := s.withPlatformSlot(ctx, client.Name())
		if lerr != nil {
			return nil, lerr
		}
		// Install the per-account gate for this attempt.  A module takes its
		// account slot right after picking a credential; when every account is
		// full it reports busy, which is returned here as backpressure.
		req.SetAccountAcquirer(func(accountID string) (func(), error) {
			return s.acquireAccountSlot(client.Name(), accountID)
		})
		// A retry reuses the same request, so drop any lease a previous attempt
		// left behind before the module runs again.
		req.ReleaseAccountSlot()
		stream, err := client.Chat(ctx, req)
		if err == nil {
			return &releaseStream{Stream: stream, release: release, account: req.ReleaseAccountSlot}, nil
		}
		release()
		req.ReleaseAccountSlot()
		lastErr = err

		// The module has already walked its whole account pool.  Re-running
		// it here would repeat the same exhausted rotation; move to the next
		// platform instead.
		if errors.Is(err, core.ErrPlatformExhausted) {
			return nil, err
		}

		f, classified := core.AsFailure(err)
		if !classified {
			return nil, err
		}

		if f.Kind == core.FailureContentBlocked {
			if guard != nil {
				guard.ReportContentBlock()
			}
			// Only the module can rewrite its own request, so the retry is
			// offered to it through the optional Degrader capability.
			if d, ok := core.AsDegrader(client); !degraded && ok && d.Degrade(req) {
				degraded = true
				s.opts.Logger.Printf("chat: client %s: content blocked, retrying with the degraded prompt", client.Name())
				if !core.SleepCtx(ctx, core.BackoffFrom(s.rotateBase(), 0)) {
					return nil, lastErr
				}
				continue
			}
			return nil, err
		}

		if !core.Retryable(f.Kind) {
			return nil, err
		}
		if f.Kind == core.FailureWAF && guard != nil && guard.ReportWAF(f.Account) {
			return nil, err
		}
		if attempt >= maxRotate {
			return nil, err
		}
		s.opts.Logger.Printf("chat: client %s: attempt %d failed (%s, account %q), rotating",
			client.Name(), attempt+1, f.Kind, f.Account)
		if !core.SleepCtx(ctx, s.rotateBackoff(attempt)) {
			return nil, lastErr
		}
	}
}

// rotateBase is the un-jittered first backoff, honouring a live override.
func (s *server) rotateBase() time.Duration {
	base := core.RotateBackoffBase
	if s.opts.Live != nil {
		if v := s.opts.Live.Load().RotateBackoffBase; v > 0 {
			base = v
		}
	}
	return base
}

// fail counts one failed request.  rec, when non-nil, is the usage record for
// the same request; marking it here keeps the counter and the traffic view
// from disagreeing.
func (s *server) fail(rec *UsageRecord) {
	s.stats.addFailure()
	if rec != nil {
		rec.Failed = true
	}
}

// errorCodeFor names a failure the way the reference panel names it in
// `error.code`: a symbolic string, never the vendor's numeric business code.
// The reference keeps the vendor's own code/msg/requestId inside `message`
// verbatim (internal/server/handler.go:912-915) and writes a word like
// "no_healthy_account", "rate_limit_exceeded", "waf_ip_blocked",
// "invalid_api_key", "invalid_request", "content_blocked" or
// "upstream_parse" here (internal/server/handler.go:142/479/767/781/795/869,
// 890/901/908).  Modules still carry the numeric code on their own error
// value -- trae uses it for failover (clients/trae/upstream.go:118) -- it
// just does not belong on the wire as a code.
//
// err may be nil: the local (pre-module) error paths only know their status.
func errorCodeFor(err error, status int) string {
	switch {
	case errors.Is(err, core.ErrNotConfigured):
		// The reference's word for "there is no usable account to send this
		// to", and also its default for local scheduling failures.
		return "no_healthy_account"
	case errors.Is(err, core.ErrBusy):
		return "rate_limit_exceeded"
	case errors.Is(err, core.ErrUnsupported):
		return "invalid_request"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "upstream_timeout"
	}
	if f, ok := core.AsFailure(err); ok {
		switch f.Kind {
		case core.FailureWAF:
			// Ours is always the IP-level block: FailureWAF is the only kind
			// that feeds the egress-IP gate (internal/core/failure.go:17-19).
			return "waf_ip_blocked"
		case core.FailureRateLimited:
			return "rate_limit_exceeded"
		case core.FailureContentBlocked:
			return "content_blocked"
		case core.FailureQuota:
			return "quota_exhausted"
		case core.FailureAuth:
			return "account_auth_failed"
		case core.FailureSessionDead:
			return "session_dead"
		case core.FailureUpstream:
			return "upstream_error"
		}
	}
	if status == http.StatusUnauthorized {
		return "invalid_api_key"
	}
	if status >= 500 {
		if err == nil {
			// A local limitation (no stream support, a handler that panicked):
			// nothing upstream failed, so do not claim it did.
			return "server_error"
		}
		return "upstream_error"
	}
	return "invalid_request"
}

// upstreamErrorShape is the (status, error type, failure kind) triple a module
// error maps onto.
//
// It is separate from writeUpstreamError because the per-request console row
// needs the status of a refused request, and a row must not have to write a
// response to learn it.
func upstreamErrorShape(err error) (int, string, core.FailureKind) {
	status := http.StatusBadGateway
	typ := "upstream_error"
	kind := core.FailureKind("")
	switch {
	case errors.Is(err, core.ErrNotConfigured):
		status, typ = http.StatusServiceUnavailable, "invalid_request_error"
	case errors.Is(err, core.ErrBusy):
		// The client is fine, it is just full.  A 429 is the answer a caller
		// can act on: back off and retry the same request, rather than
		// treating the upstream as broken.
		status, typ = http.StatusTooManyRequests, "rate_limit_error"
	case errors.Is(err, core.ErrUnsupported):
		status, typ = http.StatusBadRequest, "invalid_request_error"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		status, typ = http.StatusGatewayTimeout, "upstream_error"
	default:
		if f, ok := core.AsFailure(err); ok {
			if f.Status >= 400 && f.Status <= 599 {
				status = f.Status
			}
			if f.Kind != core.FailureOther {
				typ = string(f.Kind) + "_error"
			}
			kind = f.Kind
		}
	}
	return status, typ, kind
}

// writeUpstreamError maps a module error onto an HTTP status.  Modules are
// encouraged to wrap core.ErrNotConfigured / core.ErrUnsupported so that the
// mapping stays meaningful.
// ctx is a thunk rather than a value: resolving the hint context can cost a
// catalogue read, and only a failure ever needs one.  See hintContextFunc.
func (s *server) writeUpstreamError(w http.ResponseWriter, c core.Client, ctx func() core.HintContext, err error) {
	clientName := c.Name()
	status, typ, kind := upstreamErrorShape(err)
	if errors.Is(err, core.ErrBusy) {
		w.Header().Set("Retry-After", "1")
	}
	msg := fmt.Sprintf("[%s] %s", clientName, err.Error())
	s.opts.Logger.Printf("chat: client %s: %v", clientName, err)
	writeJSON(w, status, apiErrorEnvelope{Error: apiError{
		Message:     msg,
		Type:        typ,
		Code:        errorCodeFor(err, status),
		GatewayHint: s.gatewayHint(c, kind, msg, ctx()),
	}})
}

// gatewayHint asks the module that owns the vendor vocabulary first and falls
// back to the shared rule table in internal/hint.
//
// The order matters.  A module that implements core.HintProvider knows its own
// business codes (workbuddy's 11133/11135, for instance) and can also use the
// request context the shared table never sees.  But a module is allowed to have
// nothing to say about a given failure, and the shared table may still have a
// rule for it -- so "" from the module is a hand-off, not a verdict.
func (s *server) gatewayHint(c core.Client, kind core.FailureKind, msg string, ctx core.HintContext) string {
	if p := core.HintOf(c); p != nil {
		if h := p.Hint(kind, msg, ctx); h != "" {
			return h
		}
	}
	return hint.For(c.Name(), kind, msg)
}

// hintContextFor assembles what a module needs in order to explain an upstream
// failure: the bare model name, whether the request carried an image part, and
// what the catalogue says about that model.
//
// The catalogue lookup is skipped unless the request actually carried an image,
// because that is the only family of advice that consults it and the error path
// should not pay for a vendor call it does not need.  A catalogue that cannot
// be read, or that lists the model without stating its image capability, leaves
// ModelInCatalog false: the module then answers with the neutral wording rather
// than inventing a capability fact.
func (s *server) hintContextFor(ctx context.Context, c core.Client, model string, hasImage bool) core.HintContext {
	h := core.HintContext{Client: c.Name(), Model: model, HasImage: hasImage}
	if !h.HasImage {
		return h
	}
	models, err := c.Models(ctx)
	if err != nil {
		return h
	}
	for _, m := range models {
		if m.ID != model {
			continue
		}
		supports, stated := modelSupportsImages(m)
		if stated {
			h.ModelInCatalog = true
			h.ModelSupportsImages = supports
		}
		break
	}
	return h
}

// hintContextFunc defers the whole assembly -- including the catalogue read --
// until something actually has to be explained.  A successful request never
// calls it, so a streamed answer that fails on its last frame still pays for the
// catalogue exactly once, at the moment the frame is written.  It mirrors the
// reference's upstream.FrameHintFunc, which only runs its context function when
// an error payload shows up.
func (s *server) hintContextFunc(ctx context.Context, c core.Client, model string, msgs []core.Message) func() core.HintContext {
	hasImage := requestHasImage(msgs)
	once, cached := sync.Once{}, core.HintContext{}
	return func() core.HintContext {
		once.Do(func() {
			cached = s.hintContextFor(ctx, c, model, hasImage)
		})
		return cached
	}
}

// requestHasImage reports whether any message carries an image part.
func requestHasImage(msgs []core.Message) bool {
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Type == "image_url" || p.ImageURL != "" {
				return true
			}
		}
	}
	return false
}

// modelSupportsImages reads the capability flag a module publishes on a model.
// The second result distinguishes "the catalogue says no" from "the catalogue
// did not say", which is the difference between a truthful claim and a guess.
func modelSupportsImages(m core.Model) (value, stated bool) {
	raw, ok := m.Extra["supports_images"]
	if !ok {
		return false, false
	}
	b, ok := raw.(bool)
	return b, ok
}

// ---------------------------------------------------------------------------
// streaming
// ---------------------------------------------------------------------------

func (s *server) streamSSE(w http.ResponseWriter, r *http.Request, client core.Client, model string, stream core.Stream, includeUsage bool, rec *UsageRecord, stat *chatStat, hintCtx func() core.HintContext) {
	clientName := client.Name()
	var usage *core.Usage
	// Token counts may only arrive on the last event, and this function has
	// several early exits; one deferred copy covers all of them -- the usage
	// record and the console row both need the number.
	defer func() {
		rec.setUsage(usage)
		stat.noteUsage(usage)
	}()

	flusher, ok := w.(http.Flusher)
	if !ok {
		stat.status = http.StatusInternalServerError
		writeError(w, http.StatusInternalServerError, "server_error", "streaming unsupported by this server")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	stat.status = http.StatusOK

	id := newID("chatcmpl-")
	created := time.Now().Unix()

	send := func(choice chatChoice, usage *chatUsage) bool {
		chunk := chatResponse{
			ID:      id,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   model,
			Choices: []chatChoice{choice},
			Usage:   usage,
		}
		b, err := json.Marshal(chunk)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// role preamble, so strict clients see a well-formed first chunk
	if !send(chatChoice{Index: 0, Delta: &chatMessage{Role: "assistant"}}, nil) {
		return
	}

	var finished, sawToolCall bool
	for {
		if r.Context().Err() != nil {
			return
		}
		ev, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			s.fail(rec)
			s.opts.Logger.Printf("stream: client %s: %v", clientName, err)
			msg := fmt.Sprintf("[%s] %s", clientName, err.Error())
			_, _, kind := upstreamErrorShape(err)
			b, _ := json.Marshal(apiErrorEnvelope{Error: apiError{
				Message: msg,
				Type:    "upstream_error",
				// The stream broke mid-flight: the reference calls this
				// "upstream_parse" (internal/upstream/sse.go:601).
				Code:        "upstream_parse",
				GatewayHint: s.gatewayHint(client, kind, msg, hintCtx()),
			}})
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
			break
		}
		// The first event off the module is the first byte the caller waits
		// for; noteFirstFrame keeps only the first one.
		stat.noteFirstFrame(time.Now())

		switch ev.Type {
		case core.EventDelta:
			if ev.Delta == "" && ev.Reasoning == "" {
				continue
			}
			d := &chatMessage{}
			any := false
			if ev.Delta != "" {
				d.Content = mustJSON(ev.Delta)
				any = true
			}
			if ev.Reasoning != "" {
				d.ReasoningContent = ev.Reasoning
				any = true
			}
			if !any {
				continue
			}
			if !send(chatChoice{Index: 0, Delta: d}, nil) {
				return
			}
		case core.EventToolCall:
			if ev.ToolCall == nil {
				continue
			}
			tc := chatToolCall{
				Index: intPtr(ev.ToolCall.Index),
				ID:    ev.ToolCall.ID,
				Type:  "function",
			}
			tc.Function.Name = ev.ToolCall.Name
			tc.Function.Arguments = ev.ToolCall.Arguments
			d := &chatMessage{ToolCalls: []chatToolCall{tc}}
			sawToolCall = true
			if !send(chatChoice{Index: 0, Delta: d}, nil) {
				return
			}
		case core.EventUsage:
			if ev.Usage != nil {
				usage = ev.Usage
			}
		case core.EventDone:
			finished = true
			reason := ev.Finish
			if reason == "" {
				reason = "stop"
			}
			// Same normalisation as the buffered path: a turn that streamed a
			// tool call never ends with "stop".
			if sawToolCall && reason == "stop" {
				reason = "tool_calls"
			}
			if !send(chatChoice{Index: 0, Delta: &chatMessage{}, FinishReason: &reason}, nil) {
				return
			}
		case core.EventError:
			if ev.Err != nil {
				s.fail(rec)
				msg := fmt.Sprintf("[%s] %s", clientName, ev.Err.Error())
				_, _, kind := upstreamErrorShape(ev.Err)
				b, _ := json.Marshal(apiErrorEnvelope{Error: apiError{
					Message:     msg,
					Type:        "upstream_error",
					Code:        errorCodeFor(ev.Err, http.StatusBadGateway),
					GatewayHint: s.gatewayHint(client, kind, msg, hintCtx()),
				}})
				fmt.Fprintf(w, "data: %s\n\n", b)
				flusher.Flush()
			}
			finished = true
		}
		if finished {
			break
		}
	}

	if !finished {
		reason := "stop"
		send(chatChoice{Index: 0, Delta: &chatMessage{}, FinishReason: &reason}, nil)
	}
	if includeUsage && usage != nil {
		send(chatChoice{Index: 0, Delta: &chatMessage{}}, toWireUsage(usage))
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// ---------------------------------------------------------------------------
// buffering (stream=false)
// ---------------------------------------------------------------------------

func (s *server) bufferCompletion(w http.ResponseWriter, r *http.Request, client core.Client, model string, stream core.Stream, rec *UsageRecord, stat *chatStat, hintCtx func() core.HintContext) {
	var (
		text      strings.Builder
		reasoning strings.Builder
		toolOrder []int
		toolByIx  = map[int]*core.ToolCall{}
		usage     *core.Usage
		finish    string
		streamErr error
	)
	// Same as the streaming path: the token counts may only show up at the
	// very end, and every exit below must carry them.
	defer func() {
		rec.setUsage(usage)
		stat.noteUsage(usage)
	}()

	for {
		if r.Context().Err() != nil {
			stat.status = http.StatusGatewayTimeout
			s.fail(rec)
			writeError(w, http.StatusGatewayTimeout, "upstream_error", "client disconnected")
			return
		}
		ev, err := stream.Recv()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				streamErr = err
			}
			break
		}
		// As in the streaming path: the first event is the first byte.
		stat.noteFirstFrame(time.Now())
		switch ev.Type {
		case core.EventDelta:
			text.WriteString(ev.Delta)
			reasoning.WriteString(ev.Reasoning)
		case core.EventToolCall:
			if ev.ToolCall == nil {
				continue
			}
			tc, ok := toolByIx[ev.ToolCall.Index]
			if !ok {
				tc = &core.ToolCall{Type: "function"}
				toolByIx[ev.ToolCall.Index] = tc
				toolOrder = append(toolOrder, ev.ToolCall.Index)
			}
			if ev.ToolCall.ID != "" {
				tc.ID = ev.ToolCall.ID
			}
			if ev.ToolCall.Name != "" {
				tc.Name = ev.ToolCall.Name
			}
			tc.Arguments += ev.ToolCall.Arguments
		case core.EventUsage:
			if ev.Usage != nil {
				usage = ev.Usage
			}
		case core.EventDone:
			if ev.Finish != "" {
				finish = ev.Finish
			}
		case core.EventError:
			if ev.Err != nil {
				streamErr = ev.Err
			}
		}
	}

	if streamErr != nil {
		status, _, _ := upstreamErrorShape(streamErr)
		stat.status = status
		s.fail(rec)
		s.writeUpstreamError(w, client, hintCtx, streamErr)
		return
	}

	msg := &chatMessage{Role: "assistant"}
	if text.Len() > 0 {
		msg.Content = mustJSON(text.String())
	} else {
		// An assistant turn that produced only tool calls (or nothing at all)
		// still carries the key: OpenAI sends `"content": null`, and a client
		// that validates the message shape rejects its absence.  content is
		// json.RawMessage with omitempty, so a nil one would drop the field.
		msg.Content = json.RawMessage(`null`)
	}
	if reasoning.Len() > 0 {
		msg.ReasoningContent = reasoning.String()
	}
	sort.Ints(toolOrder)
	for _, ix := range toolOrder {
		tc := toolByIx[ix]
		if tc.Name == "" {
			continue
		}
		var wc chatToolCall
		wc.ID = tc.ID
		if wc.ID == "" {
			wc.ID = newID("call_")
		}
		wc.Type = "function"
		wc.Function.Name = tc.Name
		wc.Function.Arguments = tc.Arguments
		msg.ToolCalls = append(msg.ToolCalls, wc)
	}

	// A turn that produced tool calls does not end with "stop".  Most modules
	// say "tool_calls" themselves; Trae reports the transport-level stop while
	// the message carries the calls.  A client that branches on finish_reason
	// to decide whether to execute a call must get the same answer from every
	// module, so the gateway owns this field and normalises it here.
	if len(msg.ToolCalls) > 0 && (finish == "" || finish == "stop") {
		finish = "tool_calls"
	} else if finish == "" {
		finish = "stop"
	}

	stat.status = http.StatusOK
	writeJSON(w, http.StatusOK, chatResponse{
		ID:      newID("chatcmpl-"),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []chatChoice{{Index: 0, Message: msg, FinishReason: &finish}},
		Usage:   toWireUsage(usage),
	})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, apiErrorEnvelope{Error: apiError{
		Message: msg,
		Type:    typ,
		Code:    errorCodeFor(nil, status),
	}})
}

func mustJSON(s string) json.RawMessage {
	b, err := json.Marshal(s)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return b
}

func intPtr(i int) *int { return &i }
