package workbuddy

// realm.go — the "<realm>:<model>" addressing protocol.
//
// WorkBuddy serves two entirely separate catalogues: the domestic one behind
// copilot.tencent.com and the international one behind workbuddy.ai.  A model id
// can exist in both ("glm-5.2" is in each) and the same request routed to the
// wrong realm answers 11102 "model not found", so the realm is not decoration --
// it is the thing that decides which credential may serve the call.
//
// The reference solved this by publishing the catalogue realm-qualified and
// resolving the qualifier on the way back in (internal/server/resolve_model.go
// and modelList in internal/server/handler.go).  This file is that protocol,
// ported: a model name is "<realm>:<id>" where <realm> is exactly "cn" or
// "global"; anything else is a bare id in the domestic realm.
//
// One deliberate divergence from the reference: a bare name is a *preference*
// here, not a hard route.  The reference hard-defaults to CN, which is right for
// a single-tenant deployment that is CN-first, but this module is one of several
// in a gateway an operator may be running with only an international account.
// Hard-defaulting would turn that install's working bare-name requests into
// "no account for realm cn".  An explicit prefix is still a hard filter: if the
// operator wrote "global:glm-5.2" they get a global account or an error.

import "strings"

// resolveModelRealm splits a gateway model name into the realm it names and the
// bare model id the vendor actually knows.
//
// It is a faithful port of the reference's resolveModel: only the FIRST colon
// counts, the prefix is compared case-sensitively against exactly "cn" and
// "global", and anything else -- including a model id that legitimately contains
// a colon -- is left whole and routed to the domestic realm.  Guessing here
// would silently rewrite a model id the operator meant literally.
func resolveModelRealm(model string) (realm, bare string) {
	model = strings.TrimSpace(model)
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return realmCN, model
	}
	prefix := model[:idx]
	if prefix != realmCN && prefix != realmGlobal {
		return realmCN, model
	}
	return prefix, model[idx+1:]
}

// realmQualified reports whether the caller wrote an explicit realm prefix.
//
// It is what separates "route this to CN" from "I did not say", and the pick
// path needs the difference: an explicit prefix is a hard filter that must not
// fall back to the other realm, an absent one is a preference that may.
func realmQualified(model string) bool {
	model = strings.TrimSpace(model)
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return false
	}
	prefix := model[:idx]
	return prefix == realmCN || prefix == realmGlobal
}

// qualifyModelID renders one catalogue entry the way the gateway publishes it.
// It is the inverse of resolveModelRealm and exists so the two can never drift.
func qualifyModelID(realm, id string) string {
	if realm == "" {
		realm = realmCN
	}
	return realm + ":" + id
}

// modelRoute is one model name resolved all the way down to the decisions the
// chat path has to make: what to put on the wire, which realms may serve it and
// in what order, and what a sticky conversation binding must match.
//
// It exists so those three answers are computed in one place.  Deriving them
// separately at the call sites is how a request ends up sending a stripped id
// while the picker filters on the qualified one -- a mismatch that only shows up
// as a 11102 in production.
type modelRoute struct {
	// bare is the id the vendor knows, with any realm prefix removed.
	bare string
	// realms is the realm preference list, most preferred first.  An explicit
	// prefix yields exactly one entry (a hard route); a bare name yields the
	// domestic realm followed by "" (any), which is the soft preference.
	realms []string
	// affinityRealm is the realm a conversation binding must satisfy: the named
	// realm for a pinned name, "" (any) for a bare one.  It is deliberately not
	// "the first entry of realms", because for a bare name the first entry is a
	// preference and a binding on an international account is still valid.
	affinityRealm string
}

// routeModel resolves a gateway model name into a modelRoute.
func routeModel(model string) modelRoute {
	realm, bare := resolveModelRealm(model)
	if realmQualified(model) {
		return modelRoute{bare: bare, realms: []string{realm}, affinityRealm: realm}
	}
	return modelRoute{bare: bare, realms: []string{realmCN, ""}, affinityRealm: ""}
}
