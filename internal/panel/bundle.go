package panel

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"client2api/internal/core"
)

// bundle.go implements the whole-instance snapshot: the config file plus every
// account credential this process holds, in one portable document that another
// instance can import verbatim.
//
// It carries the *whole* config, not a platform-policy subset: a restore that
// brought the accounts back but left the pool tuning, the prompt policy, the
// panel settings and the inbound key at the target's defaults is not the same
// instance, and "why does the restored one behave differently" is not a
// question an operator should have to debug.
//
// It deliberately carries no history.  usage.json, schedule_runs.json, the
// per-account health state and the model caches are all derived or historical:
// restoring them into another instance would import stale cooldowns, not a
// working account pool.
//
// The document is plain JSON with the credentials in base64, because the point
// is to move a pool between machines.  Everything it writes back goes through
// 0600 + temp-file+rename, and an import keeps a copy of whatever it is about
// to overwrite.

const (
	// bundleKind identifies the document so an unrelated JSON file is refused
	// with a sentence instead of an unmarshalling accident.
	bundleKind = "client2api.bundle"
	// bundleVersion is the layout generation.  A build reads every version up
	// to its own and refuses anything newer, so an old binary never half-reads
	// a newer document.
	bundleVersion = 1
	// bundleFilePrefix names the download.
	bundleFilePrefix = "client2api-bundle-"
	// maxBundleFiles bounds one import.  A pool of a few hundred accounts over
	// ten platforms is a handful of files; past this the document is a mistake.
	maxBundleFiles = 4096
)

// coldConfigKeys are the config keys a running process only reads once, at
// construction: the listener, the data directory, the outbound proxy, which
// clients are registered at all, and a module's own settings.  Writing one to
// the file cannot take effect until the next start, so both the save response
// and the bundle importer say so instead of implying the change is live.
//
// Everything else (the pool tuning, cooldowns, the prompt policy, session
// stickiness, the panel settings, the timetable, model aliases, the inbound
// key) is re-applied by the hot reload.
//
// Keep in sync with CFG_RESTART_KEYS in index.html; bundle_test.go pins the two
// lists to each other so a key added on one side cannot go stale.
var coldConfigKeys = map[string]bool{
	"listen": true, "data_dir": true, "proxy": true,
	"disabled": true, "clients": true,
}

// bundleCredentialNames are the file names modules use for their credential
// store inside their own data directory.
var bundleCredentialNames = map[string]bool{
	"accounts.json":         true,
	"credentials.json":      true,
	"managed_accounts.json": true,
}

// bundleStateNames are files that live next to a credential store but are not
// one: pool health, caches, in-flight login sessions and device identities.
// They are skipped by name because importing another instance's cooldowns
// would resurrect failures that no longer exist.
var bundleStateNames = map[string]bool{
	"state.json":   true,
	"pool.json":    true,
	"models.json":  true,
	"login.json":   true,
	"device.json":  true,
	"sidecar.json": true,
}

// bundleDoc is the snapshot as it travels.
type bundleDoc struct {
	Kind       string         `json:"kind"`
	Version    int            `json:"version"`
	ExportedAt string         `json:"exported_at"`
	Source     bundleSource   `json:"source"`
	Config     map[string]any `json:"config,omitempty"`
	Files      []bundleFile   `json:"files"`
}

// bundleSource records where the document came from, so an operator holding
// two of them can tell which instance produced which.
type bundleSource struct {
	Version string   `json:"version,omitempty"`
	Clients []string `json:"clients,omitempty"`
}

// bundleFile is one credential file.  Data is []byte so encoding/json emits
// base64 and the document stays valid JSON whatever the module wrote.
type bundleFile struct {
	// Client is the module's data directory name, e.g. "cline".
	Client string `json:"client"`
	// Name is the file inside it, e.g. "accounts.json".
	Name   string `json:"name"`
	Size   int    `json:"size"`
	SHA256 string `json:"sha256"`
	Data   []byte `json:"data"`
}

// handleBundle serves GET /panel/api/bundle (export) and POST /panel/api/bundle
// (restore).  One path for both directions keeps the pair discoverable: whatever
// the export wrote is exactly what the import reads.
func (p *panel) handleBundle(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		p.bundleExport(w, r)
	case http.MethodPost:
		p.bundleImport(w, r)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "use GET to export or POST to import")
	}
}

// bundleExport answers GET /panel/api/bundle with the snapshot.
func (p *panel) bundleExport(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(p.opts.DataDir) == "" {
		writeErr(w, http.StatusBadRequest,
			"this process was started without a data directory, so there are no accounts to export")
		return
	}
	files, err := p.collectAccountFiles()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "reading accounts: "+err.Error())
		return
	}
	doc := bundleDoc{
		Kind:       bundleKind,
		Version:    bundleVersion,
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Source:     bundleSource{Version: p.opts.Version, Clients: bundleClients(files)},
		Files:      files,
	}
	if strings.TrimSpace(p.opts.ConfigPath) != "" {
		cfg, err := p.readConfigMap()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "reading config: "+err.Error())
			return
		}
		if snapshot := bundleConfigSnapshot(cfg); len(snapshot) > 0 {
			doc.Config = snapshot
		}
	}
	w.Header().Set("Content-Disposition",
		`attachment; filename="`+bundleFilePrefix+time.Now().UTC().Format("20060102-150405")+`.json"`)
	// The body is a credential dump: never let anything cache it.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, doc)
}

// collectAccountFiles walks the data directory one level deep -- every module
// owns its files directly inside its own folder -- and returns the credential
// stores it finds.
func (p *panel) collectAccountFiles() ([]bundleFile, error) {
	root := p.opts.DataDir
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []bundleFile
	total := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		client := e.Name()
		if !safeBundleSegment(client) {
			continue
		}
		dir := filepath.Join(root, client)
		names, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", client, err)
		}
		for _, n := range names {
			if n.IsDir() {
				continue
			}
			name := n.Name()
			if !bundleCredentialFile(client, name) {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w", client, name, err)
			}
			if len(out) >= maxBundleFiles {
				return nil, fmt.Errorf("more than %d credential files; refused to build a bundle this large", maxBundleFiles)
			}
			if total += len(data); total > maxBundleBytes {
				return nil, fmt.Errorf("credentials exceed %d bytes; refused to build a bundle this large", maxBundleBytes)
			}
			sum := sha256.Sum256(data)
			out = append(out, bundleFile{
				Client: client,
				Name:   name,
				Size:   len(data),
				SHA256: hex.EncodeToString(sum[:]),
				Data:   data,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Client != out[j].Client {
			return out[i].Client < out[j].Client
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// bundleCredentialFile reports whether a file directly inside a module's data
// directory holds credentials.
//
// The list is by name because every module names its store after what it is;
// anything the list does not recognise is left out rather than guessed at, so
// a new cache file can never leak into a snapshot as if it were an account.
func bundleCredentialFile(client, name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" || strings.HasPrefix(lower, ".") || !strings.HasSuffix(lower, ".json") {
		return false
	}
	if bundleStateNames[lower] {
		return false
	}
	if bundleCredentialNames[lower] {
		return true
	}
	// workbuddy keeps one document per account at the module root, named after
	// the account's uid.  Its own state file (pool.json) is filtered above.
	return strings.EqualFold(client, "workbuddy")
}

// bundleClients lists the modules that contributed a file, in order.
func bundleClients(files []bundleFile) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range files {
		if seen[f.Client] {
			continue
		}
		seen[f.Client] = true
		out = append(out, f.Client)
	}
	return out
}

// bundleConfigSnapshot copies the whole config document into the bundle.
//
// A null section is skipped rather than carried as null: the file does not
// store keys that way, and "delete this section" is not something a restore
// should ever do to the target on its own.
func bundleConfigSnapshot(cfg map[string]any) map[string]any {
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if v != nil {
			out[k] = v
		}
	}
	return out
}

// bundleConfigOrder lists a section's keys the way the config writer emits
// them, so "changed: listen, pool, prompt" reads the same on every import
// instead of following Go's randomised map order.  Keys this build does not
// know about are still carried, sorted after the known ones.
func bundleConfigOrder(section map[string]any) []string {
	keys := make([]string, 0, len(section))
	seen := make(map[string]bool, len(section))
	for _, k := range configKeyOrder {
		if _, ok := section[k]; ok {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	rest := make([]string, 0, len(section))
	for k := range section {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(keys, rest...)
}

// bundleImport answers POST /panel/api/bundle.
func (p *panel) bundleImport(w http.ResponseWriter, r *http.Request) {
	_, raw, err := readBundleUpload(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		writeErr(w, http.StatusBadRequest, "the uploaded document is empty")
		return
	}
	var doc bundleDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		writeErr(w, http.StatusBadRequest, "not a client2api bundle: "+err.Error())
		return
	}
	if doc.Kind != bundleKind {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("not a client2api bundle (kind=%q)", doc.Kind))
		return
	}
	if doc.Version <= 0 || doc.Version > bundleVersion {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf(
			"bundle version %d is not supported by this build (newest supported: %d)", doc.Version, bundleVersion))
		return
	}
	if len(doc.Files) > 0 && strings.TrimSpace(p.opts.DataDir) == "" {
		writeErr(w, http.StatusBadRequest,
			"this process was started without a data directory, so accounts cannot be restored")
		return
	}

	plan, err := bundlePlan(p.opts.DataDir, doc.Files)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	var warnings, errs []string
	backupDir := ""
	written, backedUp := 0, 0
	if len(plan) > 0 {
		backupDir = filepath.Join(p.opts.DataDir, "backups",
			"bundle-"+time.Now().UTC().Format("20060102-150405"))
		for _, f := range plan {
			if _, err := os.Stat(f.dst); err == nil {
				if err := bundleBackup(backupDir, f); err != nil {
					errs = append(errs, "backup "+f.rel+": "+err.Error())
					continue
				}
				backedUp++
			}
			if err := core.WriteFileAtomic(f.dst, f.data); err != nil {
				errs = append(errs, "write "+f.rel+": "+err.Error())
				continue
			}
			written++
		}
	}

	configApplied := false
	var appliedKeys []string
	if len(doc.Config) > 0 {
		switch {
		case strings.TrimSpace(p.opts.ConfigPath) == "":
			warnings = append(warnings,
				"the bundle carries config, but this process has no config path, so it was not applied")
		default:
			keys, err := p.applyBundleConfig(doc.Config)
			if err != nil {
				errs = append(errs, "config: "+err.Error())
			} else {
				configApplied, appliedKeys = true, keys
			}
		}
	}

	reloaded := false
	if (configApplied || written > 0) && p.opts.Reload != nil {
		if err := p.opts.Reload(); err != nil {
			warnings = append(warnings, "config was written but the hot reload failed: "+err.Error())
		} else {
			reloaded = true
		}
	}

	// A config key the running process only reads at construction cannot be
	// live, so the report says "restart" for those even when no account file
	// was touched: otherwise a restore that carried listen/proxy/clients and
	// wrote no accounts would claim it was done.
	restartRequired := written > 0
	if reloaded && p.opts.Registry != nil {
		restartRequired = false
		for _, f := range doc.Files {
			c, ok := p.opts.Registry.Get(f.Client)
			if _, hot := c.(core.SourceClient); !ok || !hot {
				restartRequired = true
				break
			}
		}
	}
	for _, k := range appliedKeys {
		if coldConfigKeys[k] {
			restartRequired = true
			break
		}
	}

	// The account files land in the directory this process is running with,
	// but the bundle's own data_dir was just merged into the file.  If the two
	// disagree, the next start reads a directory the accounts are not in, so
	// name both instead of letting the operator find an empty pool.
	if configApplied {
		if dd, ok := doc.Config["data_dir"].(string); ok && strings.TrimSpace(dd) != "" {
			if same, err := sameBundleDir(dd, p.opts.DataDir); err == nil && !same {
				warnings = append(warnings, fmt.Sprintf(
					"the bundle's data_dir is %q but this process is running with %q: the accounts were restored into the running directory, so make them agree before the next start",
					dd, p.opts.DataDir))
			}
		}
	}

	if written == 0 && len(errs) > 0 {
		writeErr(w, http.StatusInternalServerError, strings.Join(errs, "; "))
		return
	}

	res := map[string]any{
		"ok":               len(errs) == 0,
		"source":           doc.Source,
		"exported_at":      doc.ExportedAt,
		"reloaded":         reloaded,
		"restart_required": restartRequired,
		"accounts": map[string]any{
			"files":     written,
			"backed_up": backedUp,
			"clients":   bundleClients(doc.Files),
		},
		"config": map[string]any{"applied": configApplied, "keys": appliedKeys},
	}
	if backupDir != "" && backedUp > 0 {
		res["backup"] = backupDir
	}
	if len(warnings) > 0 {
		res["warnings"] = warnings
	}
	if len(errs) > 0 {
		res["errors"] = errs
	}
	writeJSON(w, http.StatusOK, res)
}

// plannedFile is one validated destination.
type plannedFile struct {
	rel  string // client/name, for messages
	dst  string
	data []byte
}

// bundlePlan validates every entry before anything is written: a document that
// names an unsafe path or fails its own checksum is refused whole, so a bad
// import cannot leave half a pool behind.
func bundlePlan(dataDir string, files []bundleFile) ([]plannedFile, error) {
	if len(files) > maxBundleFiles {
		return nil, fmt.Errorf("bundle lists %d files, more than the %d this build accepts", len(files), maxBundleFiles)
	}
	out := make([]plannedFile, 0, len(files))
	seen := map[string]bool{}
	total := 0
	for i, f := range files {
		if !safeBundleSegment(f.Client) || !safeBundleSegment(f.Name) {
			return nil, fmt.Errorf("bundle entry %d has an unsafe path (%q/%q)", i, f.Client, f.Name)
		}
		if !bundleCredentialFile(f.Client, f.Name) {
			return nil, fmt.Errorf("bundle entry %d (%s/%s) is not a credential file this build knows", i, f.Client, f.Name)
		}
		key := strings.ToLower(f.Client) + "/" + strings.ToLower(f.Name)
		if seen[key] {
			return nil, fmt.Errorf("bundle lists %s twice", f.Client+"/"+f.Name)
		}
		seen[key] = true
		if f.Size != len(f.Data) {
			return nil, fmt.Errorf("bundle entry %s claims %d bytes but carries %d", f.Client+"/"+f.Name, f.Size, len(f.Data))
		}
		if f.SHA256 != "" {
			sum := sha256.Sum256(f.Data)
			if !strings.EqualFold(hex.EncodeToString(sum[:]), f.SHA256) {
				return nil, fmt.Errorf("bundle entry %s failed its checksum", f.Client+"/"+f.Name)
			}
		}
		if total += len(f.Data); total > maxBundleBytes {
			return nil, fmt.Errorf("bundle carries more than %d bytes of credentials", maxBundleBytes)
		}
		out = append(out, plannedFile{
			rel:  f.Client + "/" + f.Name,
			dst:  filepath.Join(dataDir, f.Client, f.Name),
			data: f.Data,
		})
	}
	return out, nil
}

// bundleBackup copies the file an import is about to replace.
func bundleBackup(backupDir string, f plannedFile) error {
	old, err := os.ReadFile(f.dst)
	if err != nil {
		return err
	}
	return core.WriteFileAtomic(filepath.Join(backupDir, filepath.FromSlash(f.rel)), old)
}

// applyBundleConfig merges the bundle's config document into the config file.
// Every key the document carries is merged, and only those keys are touched: a
// snapshot from an older build that knows fewer sections never blanks one it
// has nothing to say about.
func (p *panel) applyBundleConfig(section map[string]any) ([]string, error) {
	patch := map[string]any{}
	var keys []string
	for _, k := range bundleConfigOrder(section) {
		if v := section[k]; v != nil {
			patch[k] = v
			keys = append(keys, k)
		}
	}
	if len(patch) == 0 {
		return nil, nil
	}

	// The same read-merge-validate-write cycle the config page uses.  An
	// import that wrote the file its own way could drop keys this build does
	// not know about, which is exactly what bundleConfigOrder preserves.
	if _, err := WriteConfigKeys(p.opts.ConfigPath, patch); err != nil {
		return nil, err
	}
	return keys, nil
}

// sameBundleDir reports whether two data_dir spellings name the same place.
// A relative path is resolved against the working directory, which is what the
// gateway itself does when it opens the directory.  A false "different" only
// costs one extra warning line, so this errs on the literal comparison.
func sameBundleDir(a, b string) (bool, error) {
	pa, err := filepath.Abs(a)
	if err != nil {
		return false, err
	}
	pb, err := filepath.Abs(b)
	if err != nil {
		return false, err
	}
	return filepath.Clean(pa) == filepath.Clean(pb), nil
}

// safeBundleSegment reports whether s can be used as one path element.  The
// bundle travels between machines, so "client" and "name" are never trusted:
// an absolute path, a separator or a ".." would write outside the data
// directory.
func safeBundleSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	if strings.ContainsAny(s, `/\:*?"<>|`) || strings.ContainsRune(s, 0) {
		return false
	}
	if strings.HasPrefix(s, ".") {
		return false
	}
	return s == filepath.Base(s)
}
