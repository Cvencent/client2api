package openaicompat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

func (s *source) AccountFields(context.Context) []core.FieldSpec {
	return []core.FieldSpec{
		{Key: "api_key", Label: "API Key / App Key", Type: "password", Required: true, Help: "上游的密钥。用 Basic 应用认证的上游（例如华为 APIG / AgentArts 模型网关）这里填 app key。"},
		{Key: "app_secret", Label: "App Secret（Basic 认证）", Type: "password", Advanced: true, Help: "上游用 Basic 应用认证时填这里；填了以后请求会带 Authorization: Basic base64(app_key:app_secret)。留空则继续用 Bearer。"},
		{Key: "label", Label: "备注", Type: "text"},
		{Key: "models", Label: "模型列表", Type: "textarea", Advanced: true},
		{Key: "balance_access_token", Label: "余额查询 Access Token", Type: "password", Advanced: true, Help: "部分 New API 控制台需要 Access Token 才能读取余额；聊天 API Key 能直接读时留空。"},
		{Key: "balance_user_id", Label: "余额查询 User ID", Type: "text", Advanced: true, Help: "New API 控制台的用户 ID；只有余额查询需要它时才填写。"},
	}
}
func (s *source) Accounts(context.Context) ([]core.AccountRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.pool.records(time.Now())
	for i := range out {
		if !out[i].Enabled {
			continue
		}
		if s.invalid[out[i].ID] {
			out[i].State = "invalid"
			continue
		}
		if time.Now().Before(s.cooling[out[i].ID]) {
			out[i].State = "cooling"
		}
	}
	return out, nil
}
func (s *source) AddAccount(ctx context.Context, spec core.AccountSpec) (core.AccountRecord, error) {
	return s.addAccount(ctx, spec, "")
}

// addAccount is the one write path for panel-owned credentials.  baseURL is
// normally empty, which means "use this platform's configured root"; an
// OfficeAce account passes the model gateway recorded in its own login so
// accounts minted on different gateways can coexist in one pool.
func (s *source) addAccount(ctx context.Context, spec core.AccountSpec, baseURL string) (core.AccountRecord, error) {
	key := strings.TrimSpace(spec.Fields["api_key"])
	if key == "" || strings.ContainsAny(key, " \r\n\t") {
		return core.AccountRecord{}, fmt.Errorf("api_key must be a non-empty key without whitespace")
	}
	appSecret := strings.TrimSpace(spec.Fields["app_secret"])
	if strings.ContainsAny(appSecret, " \r\n\t") {
		return core.AccountRecord{}, fmt.Errorf("app_secret must not contain whitespace")
	}
	requestedBase := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := append([]storedProvider(nil), s.rows...)
	id := spec.ID
	if !s.active {
		return core.AccountRecord{}, fmt.Errorf("source is disabled or removed")
	}
	if id == "" {
		for _, p := range rows {
			if p.APIKey == key {
				id = p.ID
				break
			}
		}
		if id == "" {
			var b [12]byte
			if _, err := rand.Read(b[:]); err != nil {
				return core.AccountRecord{}, err
			}
			id = "key-" + hex.EncodeToString(b[:])
		}
	}
	rowBase := requestedBase
	if rowBase == "" {
		rowBase = strings.TrimRight(s.cfg.BaseURL, "/")
	}
	p := storedProvider{
		ID:                 id,
		APIKey:             key,
		AppSecret:          appSecret,
		Label:              firstNonEmpty(spec.Label, spec.Fields["label"]),
		BaseURL:            rowBase,
		Models:             splitModels(spec.Fields["models"]),
		BalanceAccessToken: strings.TrimSpace(spec.Fields["balance_access_token"]),
		BalanceUserID:      strings.TrimSpace(spec.Fields["balance_user_id"]),
	}
	if spec.Enabled != nil {
		p.Disabled = !*spec.Enabled
	}
	found := false
	for i, old := range rows {
		if old.ID == id {
			// An edit made in the panel carries no gateway override.  Keep the
			// account's existing OfficeAce gateway in that case; only an
			// explicit login/import writes a new one.
			if requestedBase == "" && strings.TrimSpace(old.BaseURL) != "" {
				p.BaseURL = old.BaseURL
			}
			if old.APIKey == key && spec.Fields["models"] == "" {
				p.Models = old.Models
			}
			if spec.Enabled == nil {
				p.Disabled = old.Disabled
			}
			if p.BalanceAccessToken == "" {
				p.BalanceAccessToken = old.BalanceAccessToken
			}
			if p.BalanceUserID == "" {
				p.BalanceUserID = old.BalanceUserID
			}
			// An edit that leaves app_secret blank keeps the stored half of
			// the Basic pair; removing the account is how it is cleared.
			if p.AppSecret == "" {
				p.AppSecret = old.AppSecret
			}
			rows[i] = p
			found = true
			break
		}
	}
	if !found {
		rows = append(rows, p)
	}
	if err := s.saveLocked(rows); err != nil {
		return core.AccountRecord{}, err
	}
	delete(s.cooling, id)
	s.generation++
	for _, rec := range s.pool.records(time.Now()) {
		if rec.ID == id {
			return rec, nil
		}
	}
	return core.AccountRecord{}, core.ErrNotConfigured
}
func (s *source) RemoveAccount(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return fmt.Errorf("source is disabled or removed")
	}
	rows := []storedProvider{}
	found := false
	for _, p := range s.rows {
		if p.ID == id {
			found = true
			continue
		}
		rows = append(rows, p)
	}
	if !found {
		return fmt.Errorf("account %q not found", id)
	}
	if err := s.saveLocked(rows); err != nil {
		return err
	}
	delete(s.cooling, id)
	delete(s.lastUsed, id)
	s.generation++
	return nil
}
func (s *source) SetAccountEnabled(ctx context.Context, id string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return fmt.Errorf("source is disabled or removed")
	}
	rows := append([]storedProvider(nil), s.rows...)
	for i := range rows {
		if rows[i].ID == id {
			rows[i].Disabled = !enabled
			if err := s.saveLocked(rows); err != nil {
				return err
			}
			s.generation++
			return nil
		}
	}
	return fmt.Errorf("account %q not found", id)
}

func (s *source) TestAccount(ctx context.Context, id string) (core.TestResult, error) {
	s.mu.Lock()
	var p storedProvider
	found := false
	for _, row := range s.rows {
		if row.ID == id {
			p = row
			found = true
			break
		}
	}
	transport, prov := s.transportLocked(), s.providerLocked(p)
	generation := s.generation
	s.mu.Unlock()
	if !found {
		return core.TestResult{}, fmt.Errorf("account %q not found", id)
	}
	if len(p.Models) == 0 {
		return core.TestResult{AccountID: id, Error: "scan this platform's models first"}, nil
	}
	// probeProvider chooses the first explicit model, never a vendor default.
	res := transport.probeProvider(ctx, prov, providerRecord{ID: id, APIKey: p.APIKey, AppSecret: p.AppSecret, BaseURL: prov.BaseURL})
	if res.OK {
		s.mu.Lock()
		if s.active && s.generation == generation {
			delete(s.cooling, id)
		}
		s.mu.Unlock()
	}
	return res, nil
}

type modelCatalogueEntry struct {
	ID           string   `json:"id"`
	Output       []string `json:"output"`
	Architecture struct {
		OutputModalities []string `json:"output_modalities"`
	} `json:"architecture"`
}

func (s *source) scan(ctx context.Context, p storedProvider, transport *Client, prov ProviderConfig, generation uint64) error {
	ctx, cancel := context.WithTimeout(ctxOrBackground(ctx), transport.cfg.modelsTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, prov.base()+"/models", nil)
	if err != nil {
		return err
	}
	prov.applyHeaders(req)
	resp, err := transport.httpClient().Do(req)
	if err != nil {
		return transport.classifyUpstream(p.ID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return transport.classifyHTTP("models", p.ID, resp.StatusCode, readLimited(resp.Body, maxErrorBytes))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > maxModelsBytes {
		return fmt.Errorf("model catalogue exceeds %d bytes", maxModelsBytes)
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("invalid models response: %w", err)
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return fmt.Errorf("models response must contain a data array")
	}
	var list []modelCatalogueEntry
	if err := json.Unmarshal(env.Data, &list); err != nil {
		return fmt.Errorf("invalid models data: %w", err)
	}
	if s.scanFilter != nil {
		list, err = s.scanFilter(ctx, p, list)
		if err != nil {
			return err
		}
	}
	models := []string{}
	probeModels := []string{}
	seen := map[string]bool{}
	probed := map[string]bool{}
	for _, m := range list {
		id := strings.TrimSpace(m.ID)
		if id != "" && !seen[id] {
			seen[id] = true
			models = append(models, id)
			if modelSupportsChat(m.Output, m.Architecture.OutputModalities) && !probed[id] {
				probed[id] = true
				probeModels = append(probeModels, id)
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.accountBaseLocked(p) != prov.base() {
		return fmt.Errorf("source URL changed during model scan; scan again")
	}
	if !s.active || s.generation != generation {
		return fmt.Errorf("source or account changed during model scan; scan again")
	}
	rows := append([]storedProvider(nil), s.rows...)
	for i := range rows {
		if rows[i].ID == p.ID && rows[i].APIKey == p.APIKey {
			rows[i].Models = models
			rows[i].ProbeModels = probeModels
			rows[i].BaseURL = prov.base()
			if err := s.saveLocked(rows); err != nil {
				return err
			}
			return nil
		}
	}
	return fmt.Errorf("account changed during model scan; scan again")
}

// modelSupportsChat reports whether a model catalogue entry advertises a text
// chat output. Unknown metadata is treated as chat-capable so older providers
// keep their existing probe behaviour; an explicit non-text output is not.
func modelSupportsChat(output, outputModalities []string) bool {
	modalities := append(append([]string(nil), output...), outputModalities...)
	if len(modalities) == 0 {
		return true
	}
	for _, modality := range modalities {
		if strings.EqualFold(strings.TrimSpace(modality), "text") {
			return true
		}
	}
	return false
}
func (s *source) RefreshAccount(ctx context.Context, id string) ([]core.RefreshResult, error) {
	s.mu.Lock()
	rows := append([]storedProvider(nil), s.rows...)
	transport := s.transportLocked()
	cfg := s.cfg
	generation := s.generation
	active := s.active
	s.mu.Unlock()
	if !active {
		return nil, fmt.Errorf("source is disabled or removed")
	}
	selected := []storedProvider{}
	for _, p := range rows {
		if id != "" && p.ID != id {
			continue
		}
		if id == "" && p.Disabled {
			continue
		}
		selected = append(selected, p)
	}
	if id != "" && len(selected) == 0 {
		return nil, fmt.Errorf("account %q not found", id)
	}
	out := make([]core.RefreshResult, len(selected))
	workers := len(selected)
	if workers > 4 {
		workers = 4
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				p := selected[i]
				prov := storedToConfig(p)
				// OfficeAce stores the model gateway that belongs to each
				// Huawei login. An ordinary relay still follows the
				// platform-level URL after a config edit, but an OfficeAce
				// account must keep its own root.
				if officeAceEndpoint(s.name, cfg.BaseURL) && strings.TrimSpace(p.BaseURL) != "" {
					prov.BaseURL = strings.TrimRight(p.BaseURL, "/")
				} else {
					prov.BaseURL = cfg.BaseURL
				}
				err := s.scan(ctx, p, transport, prov, generation)
				res := core.RefreshResult{AccountID: p.ID, OK: err == nil}
				if err != nil {
					res.Error = describeError(err)
				}
				out[i] = res
			}
		}()
	}
	for i := range selected {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return out, nil
}
func (s *source) RefreshModels(ctx context.Context) ([]core.Model, error) {
	results, err := s.RefreshAccount(ctx, "")
	var failures []error
	if err != nil {
		failures = append(failures, err)
	}
	for _, r := range results {
		if !r.OK {
			failures = append(failures, fmt.Errorf("%s: %s", r.AccountID, r.Error))
		}
	}
	models, _ := s.Models(ctx)
	return models, errors.Join(failures...)
}
