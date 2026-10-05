package modelmeta

// applyOverride overlays the operator's manual values on an already-resolved
// Meta. A zero override field is left untouched so context and output can be
// restored independently.
func applyOverride(dst Meta, o Override) Meta {
	if o.ContextLength > 0 {
		dst.ContextLength = o.ContextLength
	}
	if o.MaxOutputTokens > 0 {
		dst.MaxOutputTokens = o.MaxOutputTokens
	}
	if dst.FieldSources == nil {
		dst.FieldSources = map[string]string{}
	}
	if o.ContextLength > 0 {
		dst.FieldSources[FieldContextLength] = SourceManual
	}
	if o.MaxOutputTokens > 0 {
		dst.FieldSources[FieldMaxOutputTokens] = SourceManual
	}
	dst.Source = strongestSource(dst.FieldSources)
	return dst
}
