package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// frame.go adds the cross-origin frame support a vendor sign-in needs when its
// form does not live in the top document.  Qoder CN is the motivating case: the
// phone-and-code form is an iframe on passport.aliyun.com, and Runtime.evaluate
// only reaches the execution context it is handed.
//
// The helpers below resolve a frame by URL, mint an isolated world for it and
// evaluate there.  An isolated world shares the DOM but not the page's own
// JavaScript state, which is what form filling needs -- and it keeps this driver
// from depending on script objects the vendor may rename.

// Frame is one node of the page's frame tree.
type Frame struct {
	ID   string
	URL  string
	Name string
}

type frameTree struct {
	Frame struct {
		ID   string `json:"id"`
		URL  string `json:"url"`
		Name string `json:"name"`
	} `json:"frame"`
	ChildFrames []frameTree `json:"childFrames"`
}

func flattenFrames(tree frameTree, out *[]Frame) {
	*out = append(*out, Frame{ID: tree.Frame.ID, URL: tree.Frame.URL, Name: tree.Frame.Name})
	for _, child := range tree.ChildFrames {
		flattenFrames(child, out)
	}
}

// Frames lists the page's frames, parents before children.
func (p *Page) Frames(ctx context.Context) ([]Frame, error) {
	res, err := p.call(ctx, "Page.getFrameTree", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		FrameTree frameTree `json:"frameTree"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, fmt.Errorf("decode the frame tree: %w", err)
	}
	var frames []Frame
	flattenFrames(out.FrameTree, &frames)
	return frames, nil
}

// frameContext finds the first frame whose URL contains match and mints an
// isolated world for it.
func (p *Page) frameContext(ctx context.Context, match string) (int64, string, error) {
	match = strings.TrimSpace(match)
	if match == "" {
		return 0, "", fmt.Errorf("a frame URL fragment is required")
	}
	frames, err := p.Frames(ctx)
	if err != nil {
		return 0, "", err
	}
	for _, f := range frames {
		if !strings.Contains(f.URL, match) {
			continue
		}
		res, err := p.call(ctx, "Page.createIsolatedWorld", map[string]any{
			"frameId":   f.ID,
			"worldName": "c2a",
		})
		if err != nil {
			return 0, "", fmt.Errorf("open the %s frame: %w", match, err)
		}
		var m struct {
			ExecutionContextID int64 `json:"executionContextId"`
		}
		if err := json.Unmarshal(res, &m); err != nil {
			return 0, "", fmt.Errorf("decode the frame context: %w", err)
		}
		if m.ExecutionContextID == 0 {
			return 0, "", fmt.Errorf("the %s frame did not expose an execution context", match)
		}
		return m.ExecutionContextID, f.URL, nil
	}
	return 0, "", fmt.Errorf("no frame matching %q", match)
}

// EvalInFrame runs one expression inside the first frame whose URL contains
// match and returns its value as JSON.
func (p *Page) EvalInFrame(ctx context.Context, match, expression string) (json.RawMessage, error) {
	ctxID, url, err := p.frameContext(ctx, match)
	if err != nil {
		return nil, err
	}
	res, err := p.call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    expression,
		"contextId":     ctxID,
		"returnByValue": true,
		"awaitPromise":  true,
		"userGesture":   true,
	})
	if err != nil {
		return nil, err
	}
	value, err := decodeEvalResult(res)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", firstLine(url), err)
	}
	return value, nil
}

// EvalStringInFrame runs an expression expected to produce a string.
func (p *Page) EvalStringInFrame(ctx context.Context, match, expression string) (string, error) {
	raw, err := p.EvalInFrame(ctx, match, expression)
	if err != nil {
		return "", err
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", nil
	}
	return s, nil
}

// EvalBoolInFrame runs an expression expected to produce a boolean.
func (p *Page) EvalBoolInFrame(ctx context.Context, match, expression string) (bool, error) {
	raw, err := p.EvalInFrame(ctx, match, expression)
	if err != nil {
		return false, err
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return false, nil
	}
	return v, nil
}

// ExistsInFrame reports whether a selector matches inside the frame.
func (p *Page) ExistsInFrame(ctx context.Context, match, selector string) (bool, error) {
	return p.EvalBoolInFrame(ctx, match, `!!(`+elementQueryJS(selector)+`)`)
}

// ClickInFrame dispatches a real click on the first match inside the frame.
func (p *Page) ClickInFrame(ctx context.Context, match, selector string) (bool, error) {
	js := `(function(){var el=` + elementQueryJS(selector) + `;` +
		`if(!el){return false;}el.scrollIntoView({block:'center'});el.click();return true;})()`
	return p.EvalBoolInFrame(ctx, match, js)
}

// FillInFrame sets an input inside the frame the way a human typist would, which
// is what the vendor's React-controlled form requires.
func (p *Page) FillInFrame(ctx context.Context, match, selector, value string) (bool, error) {
	js := `(function(){var el=` + elementQueryJS(selector) + `;` +
		`if(!el){return false;}` +
		`el.focus();` +
		`var win=el.ownerDocument&&el.ownerDocument.defaultView||window;` +
		`var proto=el instanceof win.HTMLTextAreaElement?win.HTMLTextAreaElement.prototype:win.HTMLInputElement.prototype;` +
		`var setter=Object.getOwnPropertyDescriptor(proto,'value').set;` +
		`setter.call(el,` + jsString(value) + `);` +
		`el.dispatchEvent(new Event('input',{bubbles:true}));` +
		`el.dispatchEvent(new Event('change',{bubbles:true}));` +
		`return true;})()`
	return p.EvalBoolInFrame(ctx, match, js)
}

// WaitForInFrame polls an expression inside the frame until it is truthy or the
// deadline passes.
func (p *Page) WaitForInFrame(ctx context.Context, match, expression string, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		ok, err := p.EvalBoolInFrame(ctx, match, expression)
		if err == nil && ok {
			return true, nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return false, err
			}
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// WaitFrameFor polls the frame tree until a frame matches, which is what a page
// that swaps its login iframe in needs.
func (p *Page) WaitFrameFor(ctx context.Context, match string, timeout time.Duration) (Frame, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		frames, err := p.Frames(ctx)
		if err == nil {
			for _, f := range frames {
				if strings.Contains(f.URL, match) {
					return f, nil
				}
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return Frame{}, lastErr
			}
			return Frame{}, fmt.Errorf("no frame matching %q appeared", match)
		}
		select {
		case <-ctx.Done():
			return Frame{}, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// decodeEvalResult unwraps one Runtime.evaluate answer.  Both the page-level and
// the frame-level helpers share it so a script error is reported the same way.
func decodeEvalResult(res json.RawMessage) (json.RawMessage, error) {
	var out struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, fmt.Errorf("decode the page result: %w", err)
	}
	if out.ExceptionDetails != nil {
		desc := out.ExceptionDetails.Text
		if out.ExceptionDetails.Exception != nil && out.ExceptionDetails.Exception.Description != "" {
			desc = out.ExceptionDetails.Exception.Description
		}
		return nil, fmt.Errorf("the page script failed: %s", firstLine(desc))
	}
	if len(out.Result.Value) == 0 {
		return json.RawMessage("null"), nil
	}
	return out.Result.Value, nil
}
