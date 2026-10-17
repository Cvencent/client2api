package modelmeta

// ApplyOverride overlays an operator override on an already-resolved Meta. It is
// the public form of applyOverride for callers that need to reapply the manual
// layer after a deliberate capability-source overlay.
func ApplyOverride(dst Meta, o Override) Meta {
	return applyOverride(dst, o)
}

// applyOverride overlays the operator's manual values on an already-resolved
// Meta. A zero override field is left untouched so context and output can be
// restored independently. Price flags are grouped: when a manual price is
// present it replaces both input/output and the cache-read decision together.
func applyOverride(dst Meta, o Override) Meta {
	if dst.FieldSources == nil {
		dst.FieldSources = map[string]string{}
	}
	if o.ContextLength > 0 {
		dst.ContextLength = o.ContextLength
		dst.FieldSources[FieldContextLength] = SourceManual
	}
	if o.MaxOutputTokens > 0 {
		dst.MaxOutputTokens = o.MaxOutputTokens
		dst.FieldSources[FieldMaxOutputTokens] = SourceManual
	}
	if len(o.SupportedEfforts) > 0 {
		dst.Efforts = append([]string(nil), o.SupportedEfforts...)
		dst.FieldSources[FieldEfforts] = SourceManual
	}
	if o.DefaultEffort != "" {
		dst.DefaultEffort = o.DefaultEffort
		dst.FieldSources[FieldDefaultEffort] = SourceManual
	}
	if o.HasPrice {
		dst.InputPerMillion = o.InputPerMillion
		dst.OutputPerMillion = o.OutputPerMillion
		dst.CacheReadPerMillion = o.CacheReadPerMillion
		dst.HasPrice = true
		dst.HasCacheRead = o.HasCacheRead
		dst.FieldSources[FieldInputPrice] = SourceManual
		dst.FieldSources[FieldOutputPrice] = SourceManual
		if o.HasCacheRead {
			dst.FieldSources[FieldCacheReadPrice] = SourceManual
		} else {
			delete(dst.FieldSources, FieldCacheReadPrice)
		}
	}
	if dst.DefaultEffort != "" && !containsString(dst.Efforts, dst.DefaultEffort) {
		dst.DefaultEffort = ""
		delete(dst.FieldSources, FieldDefaultEffort)
	}
	dst.Source = strongestSource(dst.FieldSources)
	return dst
}
