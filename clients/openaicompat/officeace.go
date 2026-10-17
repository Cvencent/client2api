package openaicompat

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"client2api/internal/core"
	"client2api/internal/smscap"
)

// The platform this build ships with.  Its model gateway is the Huawei
// AgentArts root the OfficeAce client logs in to; the operator's config can
// override the label and the URL, but the platform itself needs no entry.
const (
	officeAceSourceID       = "officeace"
	officeAceSourceLabel    = "华为"
	officeAceDefaultBaseURL = "https://modelgw-0009.officeace.cn-southwest-2.huaweicloud-agentarts.com/v2"
)

// officeAceSource is a platform that talks to an OfficeAce / Huawei AgentArts
// model gateway.  Two things set it apart from an ordinary relay: it can mint
// its own credentials with the Huawei account login (see officeace_login.go),
// and each of its accounts may live on its own model gateway, so the account's
// URL wins over the platform default.
//
// The wrapper is applied only to relays whose id or host says OfficeAce, so
// every other custom relay keeps exactly its previous surface.
type officeAceSource struct {
	*source

	// eps are the Huawei endpoints.  A zero value falls back to the shipped
	// defaults; tests point them at their own server.
	eps officeAceEndpoints

	loginMu sync.Mutex
	logins  map[string]*officeAceLoginSession

	refreshMu    sync.Mutex
	refreshLocks map[string]*sync.Mutex

	// SMS and auto-login state.  The platform token and per-call overrides
	// travel through core.SMSOpts/core.AutoLoginRequest, so no credential is
	// added to SourceConfig.  smsBase is only set by tests.
	smsBase string
	smsRot  smscap.Rotator

	autoMu     sync.Mutex
	autoJobs   map[string]*officeAceAutoJob
	autoDriver officeAceAutoDriver
}

// officeAceLocalSource is the wrapper that can additionally read the logins
// the desktop client already stored.  It exists as its own type because the
// capability is a type assertion: the import tab must stay hidden on a build
// that cannot unwrap the client's DPAPI-protected store, while the account
// login keeps working there.
type officeAceLocalSource struct{ *officeAceSource }

func (o *officeAceSource) officeAceRefreshLock(id string) *sync.Mutex {
	o.refreshMu.Lock()
	defer o.refreshMu.Unlock()
	if o.refreshLocks == nil {
		o.refreshLocks = map[string]*sync.Mutex{}
	}
	lock := o.refreshLocks[id]
	if lock == nil {
		lock = &sync.Mutex{}
		o.refreshLocks[id] = lock
	}
	return lock
}

func wrapOfficeAceSource(name string, cfg core.SourceConfig, s *source) core.SourceClient {
	if s == nil || !officeAceEndpoint(name, cfg.BaseURL) {
		return s
	}
	oa := &officeAceSource{source: s, eps: defaultOfficeAceEndpoints()}
	oa.enableDesktopModelSelection()
	if !officeAceLocalSupported() {
		return oa
	}
	return &officeAceLocalSource{officeAceSource: oa}
}

// officeAceEndpoint reports whether a custom relay is meant to be the OfficeAce
// model gateway.  The id is the operator's own choice; the host is what the
// OfficeAce client stores in its profile.
func officeAceEndpoint(name, baseURL string) bool {
	if strings.EqualFold(strings.TrimSpace(name), "officeace") {
		return true
	}
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u == nil {
		return false
	}
	return strings.Contains(strings.ToLower(u.Hostname()), "officeace")
}

// officeAceSecureDirs lists the profile stores the OfficeAce client may use.
// The desktop runtime's working directory wins (packages/api); the packaged
// green copy and the legacy HOME fallback stay for already-migrated installs.
func officeAceSecureDirs() []string {
	var out []string
	seen := map[string]bool{}
	add := func(dir string) {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			return
		}
		dir = filepath.Clean(dir)
		key := strings.ToLower(dir)
		if seen[key] {
			return
		}
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			return
		}
		if len(officeAceProfileFiles(dir)) == 0 {
			return
		}
		seen[key] = true
		out = append(out, dir)
	}
	if dir := strings.TrimSpace(os.Getenv(officeAceSecureDirEnv)); dir != "" {
		add(dir)
		// The client itself treats the override as exclusive: when it is set,
		// that directory is the whole profile store.  Honouring the same rule
		// keeps an overridden install from also reading the real one.
		return out
	}
	rel := filepath.FromSlash(officeAceSecureDirRel)
	for _, root := range officeAceInstallDirs() {
		add(filepath.Join(root, "packages", "api", rel))
		add(filepath.Join(root, "packages", "green-package", "api", rel))
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, rel))
	}
	return out
}

// officeAceCandidates reads every OfficeAce profile it can find.  A profile
// whose key or envelope cannot be read is still reported, with the reason, so
// the panel can explain the failure instead of showing an empty list.
func officeAceCandidates() []officeAceCandidate {
	var out []officeAceCandidate
	for _, dir := range officeAceSecureDirs() {
		files := officeAceProfileFiles(dir)
		if len(files) == 0 {
			continue
		}
		secret, keyErr := officeAceInstallSecret(dir)
		var key []byte
		if keyErr == nil {
			key = officeAceProfileKey(secret)
		}
		for _, file := range files {
			cand := officeAceCandidate{path: file.path, err: keyErr}
			if keyErr == nil {
				cand.cred, cand.err = officeAceReadProfile(file, key)
			}
			out = append(out, cand)
		}
	}
	return out
}

func officeAceInstallSecret(dir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, officeAceKeyFileName))
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", officeAceKeyFileName, err)
	}
	return parseOfficeAceInstallSecret(raw, unprotectOfficeAceDPAPI)
}

func officeAceReadProfile(file officeAceProfileFile, key []byte) (officeAceCredential, error) {
	raw, err := os.ReadFile(file.path)
	if err != nil {
		return officeAceCredential{}, err
	}
	plain, err := decryptOfficeAceProfile(raw, key)
	if err != nil {
		return officeAceCredential{}, err
	}
	return parseOfficeAceCredential(plain, file.path, file.userID)
}

// Discover lists the OfficeAce logins this machine already holds.  It is
// read-only and never returns the key pair itself.
func (s *officeAceLocalSource) Discover(ctx context.Context) ([]core.DiscoveredCredential, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	srcBase := normalizeOfficeAceBaseURL(s.source.currentBaseURL())
	out := []core.DiscoveredCredential{}
	for _, cand := range officeAceCandidates() {
		dc := core.DiscoveredCredential{Path: cand.path, Kind: "officeace-oauth-profile"}
		if cand.err != nil {
			dc.Label = officeAceLabelFromPath(cand.path)
			dc.Note = "无法读取 OfficeAce 凭据：" + cand.err.Error()
			out = append(out, dc)
			continue
		}
		cred := cand.cred
		dc.Label = cred.label()
		if !cred.usable() {
			dc.Note = "该 OfficeAce 登录没有模型网关信息（model_app_key / model_app_secret）"
			out = append(out, dc)
			continue
		}
		mismatch := srcBase != "" && cred.BaseURL != "" && srcBase != cred.BaseURL
		dc.Note = officeAceNote(cred, srcBase, mismatch)
		// Each OfficeAce account carries the model gateway the client recorded
		// for it, so an account on another gateway is imported with its own
		// URL instead of being refused.
		dc.Importable = true
		dc.Imported = s.source.hasCredential(cred.AppKey, cred.AppSecret)
		out = append(out, dc)
	}
	return out, nil
}

// Import adopts the named OfficeAce logins, or every importable one when all is
// true.  The key pair lands in this relay's own account store; the OfficeAce
// client keeps its copy.
func (s *officeAceLocalSource) Import(ctx context.Context, paths []string, all bool) ([]core.AccountRecord, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(paths) == 0 && !all {
		// CredentialImporter contract: an empty selection that did not ask for
		// "all" is a no-op, not an implicit import everything.
		return nil, nil
	}
	cands := officeAceCandidates()
	var selected []officeAceCandidate
	if all {
		for _, cand := range cands {
			if officeAceSelectable(cand) {
				selected = append(selected, cand)
			}
		}
		if all && len(selected) == 0 {
			return nil, errors.New("officeace: no importable OfficeAce credential found on this machine")
		}
	} else {
		byPath := make(map[string]officeAceCandidate, len(cands))
		for _, cand := range cands {
			byPath[filepath.Clean(cand.path)] = cand
		}
		for _, p := range paths {
			p = strings.TrimSpace(p)
			cand, ok := byPath[filepath.Clean(p)]
			if !ok {
				selected = append(selected, officeAceCandidate{path: p, err: errors.New("no such OfficeAce credential file")})
				continue
			}
			selected = append(selected, cand)
		}
	}
	var (
		out      []core.AccountRecord
		failures []string
	)
	for _, cand := range selected {
		label := officeAceLabelFromPath(cand.path)
		if cand.err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", cand.path, cand.err))
			continue
		}
		cred := cand.cred
		if !cred.usable() {
			failures = append(failures, fmt.Sprintf("%s: the OfficeAce login carries no model_app_key / model_app_secret pair", cand.path))
			continue
		}
		rec, err := s.source.addAccount(ctx, core.AccountSpec{
			ID:    cred.accountID(),
			Label: firstNonEmpty(cred.label(), label),
			Fields: map[string]string{
				"api_key":    cred.AppKey,
				"app_secret": cred.AppSecret,
			},
		}, cred.BaseURL)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", cand.path, err))
			continue
		}
		if cred.Session != nil {
			if err := s.saveOfficeAceSession(rec.ID, *cred.Session); err != nil {
				s.source.deps.Log("%s: imported %s, but its cloud session could not be stored: %v", officeAceSourceID, rec.ID, core.Redact(err.Error()))
			}
		}
		out = append(out, rec)
	}
	if len(out) == 0 {
		if len(failures) > 0 {
			return nil, errors.New(strings.Join(failures, "; "))
		}
		return nil, errors.New("officeace: no OfficeAce credential was selected")
	}
	return out, nil
}

func officeAceSelectable(cand officeAceCandidate) bool {
	return cand.err == nil && cand.cred.usable()
}

func officeAceNote(cred officeAceCredential, srcBase string, mismatch bool) string {
	var b strings.Builder
	b.WriteString("OfficeAce 客户端（华为账号 OAuth）")
	if cred.BaseURL != "" {
		b.WriteString("；上游 " + cred.BaseURL)
	}
	if mismatch {
		b.WriteString("；与本平台的默认地址（" + srcBase + "）不同，导入后这个账号走它自己的网关地址")
	} else {
		b.WriteString("；导入后点“扫描模型”。Key 在客户端重新登录并轮换后可再次导入覆盖")
	}
	return b.String()
}

func officeAceLabelFromPath(path string) string {
	name := strings.TrimSuffix(filepath.Base(path), officeAceFileSuffix)
	name = strings.TrimPrefix(name, officeAceFilePrefix)
	if decoded := officeAceDecodeUserID(name); decoded != "" {
		return truncate(decoded, 40)
	}
	return filepath.Base(path)
}

// --- small helpers ----------------------------------------------------------

func (s *source) currentBaseURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.BaseURL
}

// hasCredential reports whether the pool already holds this exact pair.  It
// compares secrets in memory only; nothing is written or logged.
func (s *source) hasCredential(appKey, appSecret string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range s.rows {
		if row.APIKey == appKey && row.AppSecret == appSecret {
			return true
		}
	}
	return false
}
