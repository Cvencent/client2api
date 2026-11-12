package qoder

import "strings"

// realm.go -- Qoder's two upstream families and the "<realm>:<model>" naming
// protocol between them.
//
// Qoder runs the same product twice.  The mainland build (qoder.com.cn) keeps
// its credentials in %APPDATA%\com.qodercn.app.stable and talks to
// openapi.qoder.com.cn / gateway.qoder.com.cn.  The international build
// (qoder.com) keeps its credentials in %APPDATA%\com.qoder.app.stable and talks
// to openapi.qoder.sh, with a separate model catalogue and a separate credit
// ledger.
//
// The two catalogues do not overlap: the CN build publishes qmodel_38max /
// qfmodel, the international one publishes auto / ultimate / fast / efficient /
// smodel / cmodel.  A credential from one realm can never serve the other
// realm's models, so the realm is not decoration -- it decides which account may
// serve a call, exactly like the WorkBuddy realms do (see
// clients/workbuddy/realm.go, which this file ports).
//
// Addressing: a gateway model name is "<realm>:<id>", where <realm> is exactly
// "cn" or "global".  Anything else is a bare id in the module's default realm.
// One deliberate divergence from the WorkBuddy port: a bare name is a soft
// preference, not a hard route.  A single-realm operator (the common case, and
// the only case the module supported before this file) must keep working with
// the bare ids they already have in their config, so a bare name is ordered
// domestic-first and then allowed to fall through to any realm.

const (
	// Realm codes.  These are the wire spelling the gateway and the panel both
	// use, and the value stored on each account.
	realmCN     = "cn"
	realmGlobal = "global"
)

// RealmName normalises one realm spelling.  The desktop builds and the product
// documentation spell the international realm "global" and "intl"
// interchangeably, so both are accepted; anything unrecognised falls back to
// the domestic realm, which is what a credential that predates this file is.
func normalizeRealm(realm string) string {
	switch strings.ToLower(strings.TrimSpace(realm)) {
	case "global", "intl", "international", "overseas":
		return realmGlobal
	default:
		return realmCN
	}
}

// resolveModelRealm splits a gateway model name into the realm it names and the
// bare model id the vendor actually knows.  Only the FIRST colon counts, the
// prefix is compared against exactly the realm codes, and anything else --
// including a model id that legitimately contains a colon -- is left whole.
func resolveModelRealm(model string) (realm, bare string) {
	model = strings.TrimSpace(model)
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "", model
	}
	prefix := model[:idx]
	if prefix != realmCN && prefix != realmGlobal {
		return "", model
	}
	return prefix, model[idx+1:]
}

// realmQualified reports whether the caller wrote an explicit realm prefix.  It
// is what separates "route this to cn" from "I did not say", and the candidate
// picker needs the difference: an explicit prefix is a hard filter, an absent
// one is a preference.
func realmQualified(model string) bool {
	realm, _ := resolveModelRealm(model)
	return realm != ""
}

// qualifyModelID renders one catalogue entry the way the gateway publishes it.
// It is the inverse of resolveModelRealm, so the two can never drift.
func qualifyModelID(realm, id string) string {
	return normalizeRealm(realm) + ":" + id
}

// modelRoute is one model name resolved down to the decisions the chat path has
// to make: what to put on the wire, which realms may serve it and in what
// order.  Computing them in one place is what stops a request from sending a
// stripped id while the picker filtered on the qualified one.
type modelRoute struct {
	// bare is the id the vendor knows, with any realm prefix removed.
	bare string
	// realms is the realm preference list, most preferred first.  An explicit
	// prefix yields exactly one entry (a hard route); a bare name yields the
	// module's default realm first, then "" (any), which is the soft
	// preference.
	realms []string
	// affinityRealm is the realm a conversation binding must satisfy: the named
	// realm for a pinned name, "" (any) for a bare one.
	affinityRealm string
}

// routeModel resolves a gateway model name into a modelRoute.  defaultRealm is
// the realm a bare name prefers.
func routeModel(model, defaultRealm string) modelRoute {
	realm, bare := resolveModelRealm(model)
	if realm != "" {
		return modelRoute{bare: bare, realms: []string{realm}, affinityRealm: realm}
	}
	return modelRoute{bare: bare, realms: []string{normalizeRealm(defaultRealm), ""}, affinityRealm: ""}
}

// realmOf returns the realm an account belongs to.  An account that predates
// the realm field, or that was added without saying, is domestic: that is the
// only realm the module shipped with.
func (a *account) realmOf() string {
	if a == nil {
		return realmCN
	}
	return normalizeRealm(a.Realm)
}
