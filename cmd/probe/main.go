// Command probe measures the real output ceiling of an OpenAI-compatible
// endpoint, and separates the two ways a long answer can end.
//
// The problem it exists for: many gateways publish a max_output_tokens in
// /v1/models that does not match reality (one measured gateway claimed 393216
// for a model that really stopped at 32000), and the mismatch is usually a
// *silent* clamp — asking for more than the real ceiling is not an error, the
// upstream just truncates.  usage alone cannot tell you which happened, so this
// tool forces a long answer and climbs a ladder of max_tokens values, reading
// finish_reason at each rung:
//
//	finish=length and out <  requested  → the upstream clamped; out IS the ceiling
//	finish=length and out == requested  → supports at least this much; climb higher
//	finish=stop   and out <  requested  → the model chose to stop; no conclusion,
//	                                      retry with a firmer prompt
//
// Ported from the reference scripts/probe_max_tokens.py.  The reference ships it
// as a Python script; this tree is pure Go with no script runtime, so it is a
// second small binary instead — the same reason the panel is a Go handler and
// not a Node app.  cmd/client2api does not import it and it imports nothing from
// the gateway — only internal/core, for the shared panic guard on its worker
// goroutines: the operator runs it by hand against a running gateway whenever
// they want the models view's "measured" column to say something.
//
// The only contract with the gateway is one file, written when --panel-out is
// given, and it is exactly what internal/panel/modelprobes.go relays:
//
//	{"version":1,"probes":{"cn:glm-5.2":{"claimed":131072,"measured":48000,
//	 "verdict":"clamped","note":"...","tested_at":"...","source":"cmd/probe"}}}
//
// Writing it is a merge, not a replacement: a model that was not retested keeps
// its previous record, so a partial run narrows the file rather than emptying
// it.  A probe run takes effect on the next panel query; the gateway is never
// restarted.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// probeSource is stamped into every record so a reader can tell which tool (and
// which revision of it) produced a measurement.
const probeSource = "cmd/probe"

// ---------------------------------------------------------------------------
// Prompts
// ---------------------------------------------------------------------------

// longPrompt asks for a long, boring, unsummarisable answer.
//
// The target has to be the same order of magnitude as the rung being probed.
// Measured lesson from the reference: a target that is absurdly large (count to
// 999999) makes the model decide the task is impossible and stop after a few
// thousand tokens, while a target in the 12000–20000 range looks finishable and
// keeps it generating until the upstream hard-truncates — which is the whole
// point.  So the target scales with the rung: about max_tokens/3 lines, since a
// line is roughly 2–3 tokens, leaving headroom so the model does not run out of
// numbers before the upstream runs out of budget.
func longPrompt(maxTokens int) string {
	target := maxTokens / 3
	if target < 2000 {
		target = 2000
	}
	if target > 200000 {
		target = 200000
	}
	return fmt.Sprintf(
		"请从 1 开始，每行输出一个数字，依次递增，一直数到 %d。"+
			"严格只输出数字本身，每行一个；不要解释、不要总结、不要省略、不要合并、不要提前结束。", target)
}

// longPromptHard is the retry wording, used after the model stopped on its own.
// It switches language because a model that ignored the instruction once tends
// to ignore the same instruction again; the target stays rung-matched.
func longPromptHard(maxTokens int) string {
	target := maxTokens / 3
	if target < 2000 {
		target = 2000
	}
	if target > 200000 {
		target = 200000
	}
	return fmt.Sprintf(
		"Count from 1 to %d, one number per line, incrementing by 1. "+
			"Output digits only, one per line. Do not summarize, do not explain, "+
			"do not skip numbers, do not stop early — you must reach the target.", target)
}

// ---------------------------------------------------------------------------
// Model listing
// ---------------------------------------------------------------------------

// modelTarget is one row of /v1/models, reduced to what probing needs.  Both
// claimed fields are pointers because "the vendor said nothing" and "the vendor
// said 0" are different answers and only the first is common.
type modelTarget struct {
	ID            string
	ClaimedOutput *int
	ClaimedCtx    *int
}

// modelListResponse tolerates both shapes seen in the wild: a bare array, and
// an object wrapping it in "data".
type modelListResponse struct {
	Data []json.RawMessage `json:"data"`
}

// fetchModels reads the model catalogue.
//
// It is deliberately permissive about entry shape — a string entry, a missing
// id, a null top_provider — because this tool's job is to measure, and refusing
// to run over a cosmetic catalogue quirk would defeat it.
func fetchModels(client *http.Client, base, key string) ([]modelTarget, error) {
	url := strings.TrimRight(base, "/") + "/models"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("x-api-key", key)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d: %s", url, resp.StatusCode, clip(string(body), 300))
	}

	// An array at the top level, or {"data": [...]}.
	trimmed := bytes.TrimSpace(body)
	var entries []json.RawMessage
	if len(trimmed) > 0 && trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &entries); err != nil {
			return nil, fmt.Errorf("parse %s: %w", url, err)
		}
	} else {
		var wrapper modelListResponse
		if err := json.Unmarshal(trimmed, &wrapper); err != nil {
			return nil, fmt.Errorf("parse %s: %w", url, err)
		}
		entries = wrapper.Data
	}

	out := make([]modelTarget, 0, len(entries))
	for _, raw := range entries {
		// A bare string is a model id and nothing else.
		var asString string
		if err := json.Unmarshal(raw, &asString); err == nil {
			if id := strings.TrimSpace(asString); id != "" {
				out = append(out, modelTarget{ID: id})
			}
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			continue
		}
		id := strings.TrimSpace(firstString(obj, "id", "name"))
		if id == "" {
			continue
		}
		t := modelTarget{ID: id}
		t.ClaimedOutput = firstInt(obj, "max_output_tokens", "max_completion_tokens")
		t.ClaimedCtx = firstInt(obj, "context_length", "max_input_tokens")
		// This gateway nests a module's per-model facts under "extra" (see
		// internal/gateway/openai.go modelEntry.Extra), so a claim published
		// there is the normal case here, not an exotic one.  Reading only the
		// top level made the ladder report "声称 -" for every model of a
		// gateway that does publish max_output_tokens.
		if extra, ok := obj["extra"].(map[string]any); ok {
			if t.ClaimedOutput == nil {
				t.ClaimedOutput = firstInt(extra, "max_output_tokens", "max_completion_tokens")
			}
			if t.ClaimedCtx == nil {
				t.ClaimedCtx = firstInt(extra, "context_length", "max_input_tokens")
			}
		}
		if t.ClaimedOutput == nil {
			if top, ok := obj["top_provider"].(map[string]any); ok {
				t.ClaimedOutput = firstInt(top, "max_completion_tokens")
			}
		}
		out = append(out, t)
	}
	return out, nil
}

func firstString(obj map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := obj[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// firstInt returns the first key that carries a usable number.  It reads through
// json.Number as well as float64 so the same helper works on both a
// map[string]any built by encoding/json and one built by hand in a test.
func firstInt(obj map[string]any, keys ...string) *int {
	for _, k := range keys {
		switch v := obj[k].(type) {
		case float64:
			n := int(v)
			return &n
		case json.Number:
			if n, err := v.Int64(); err == nil {
				i := int(n)
				return &i
			}
		case int:
			n := v
			return &n
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// One request
// ---------------------------------------------------------------------------

// probeSample is what a single streaming completion told us.  HasOut is separate
// from Out because a response with no usage block at all is not a response that
// reported zero tokens, and the ladder logic turns on that distinction.
type probeSample struct {
	Out       int
	Reasoning int
	HasOut    bool
	Finish    string
}

// probeOnce sends one forced-long-output request and reads the SSE stream to the
// end, keeping the last usage block and the last finish_reason it sees.
//
// It streams rather than buffering because a rung can legitimately produce tens
// of thousands of tokens; the answer is only in the final usage frame anyway.
func probeOnce(client *http.Client, base, key, model string, maxTokens int, prompt string) (probeSample, error) {
	var sample probeSample
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"stream":     true,
		"max_tokens": maxTokens,
		"messages":   []map[string]any{{"role": "user", "content": prompt}},
		// The answer is read out of the final usage frame, and an
		// OpenAI-compatible gateway is free to send that frame only when the
		// request asks for it — this one does exactly that.  Without this the
		// ladder saw finish=length with no token count and could not conclude
		// anything at all (live: a 138 s rung came back with a blank verdict).
		"stream_options": map[string]any{"include_usage": true},
	})
	if err != nil {
		return sample, err
	}
	url := strings.TrimRight(base, "/") + "/chat/completions"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return sample, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("x-api-key", key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := client.Do(req)
	if err != nil {
		return sample, fmt.Errorf("%s", clip(err.Error(), 200))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return sample, fmt.Errorf("HTTP %d: %s", resp.StatusCode, clip(string(detail), 300))
	}

	scanner := bufio.NewScanner(resp.Body)
	// A single SSE frame can carry a large text delta; the default 64 KB token
	// limit is not enough headroom for a chatty upstream.
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var frame struct {
			Usage *struct {
				CompletionTokens int `json:"completion_tokens"`
				Details          *struct {
					ReasoningTokens int `json:"reasoning_tokens"`
				} `json:"completion_tokens_details"`
			} `json:"usage"`
			Choices []struct {
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			continue
		}
		if frame.Usage != nil {
			sample.Out = frame.Usage.CompletionTokens
			sample.HasOut = true
			if frame.Usage.Details != nil {
				sample.Reasoning = frame.Usage.Details.ReasoningTokens
			}
		}
		for _, c := range frame.Choices {
			if c.FinishReason != "" {
				sample.Finish = c.FinishReason
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return sample, fmt.Errorf("%s", clip(err.Error(), 200))
	}
	return sample, nil
}

// ---------------------------------------------------------------------------
// The ladder
// ---------------------------------------------------------------------------

// probeResult is one model's record, in the same shape the reference wrote to
// its JSONL so an existing results file stays resumable.
type probeResult struct {
	Model         string   `json:"model"`
	Measured      *int     `json:"measured"`
	Verdict       string   `json:"verdict"`
	Evidence      []string `json:"evidence"`
	MaxFull       int      `json:"max_full,omitempty"`
	Elapsed       float64  `json:"elapsed"`
	ClaimedOutput *int     `json:"claimed_output"`
	ClaimedCtx    *int     `json:"claimed_ctx"`
	Time          string   `json:"time"`
}

// probeModel climbs the rungs for one model.
//
// budget caps the wall time spent on a single model.  Without it one model with
// a genuinely huge ceiling can hold the whole run: the reference measured a
// model that needed 720 s to emit 40000 tokens, so a 100000-token rung would
// have cost half an hour on its own.  When the budget runs out the result is
// "未测完" with the lower bound discovered so far, which is still a useful
// answer.
func probeModel(client *http.Client, base, key, model string, tiers []int, retries int, quiet bool, budget time.Duration, claimed *int) probeResult {
	res := probeResult{Model: model, Evidence: []string{}, ClaimedOutput: claimed}
	started := time.Now()
	elapsed := func() float64 { return math.Round(time.Since(started).Seconds()*10) / 10 }
	allFailed := true
	skipped := false

	// retries counts the RETRIES after a finish=stop, so zero must still send one
	// request.  The loop used to run `attempt <= retries`, which turned
	// `-retries 0` into "send nothing at all" and then reported the model as
	// unavailable — the exact false verdict the allFailed branch below warns
	// about, and with an empty evidence list to hide the reason.
	attempts := retries
	if attempts < 1 {
		attempts = 1
	}

	for _, tier := range tiers {
		if budget > 0 && time.Since(started) > budget {
			res.Verdict = fmt.Sprintf("未测完（超出时间预算 %ds；已知至少 >%d）", int(budget.Seconds()), res.MaxFull)
			res.Elapsed = elapsed()
			return res
		}
		for attempt := 1; attempt <= attempts; attempt++ {
			prompt := longPrompt(tier)
			if attempt > 1 {
				prompt = longPromptHard(tier)
			}
			// A single request is bounded by what is left of the budget too, so
			// one slow rung cannot overshoot the cap it was given.  The
			// shortened deadline lives in a per-attempt copy: writing it back
			// onto the shared client would shrink every later attempt as well.
			reqClient := client
			if budget > 0 {
				remain := budget - time.Since(started)
				if remain <= 5*time.Second {
					// Too little budget left to be worth starting a request that
					// the ladder could not finish reading.  Recorded separately
					// from a failure: "the upstream never answered" and "you gave
					// me less than five seconds" need different fixes.
					skipped = true
					break
				}
				if remain < client.Timeout {
					reqClient = withTimeout(client, remain)
				}
			}
			t0 := time.Now()
			sample, err := probeOnce(reqClient, base, key, model, tier, prompt)
			dt := time.Since(t0)
			if err != nil {
				res.Evidence = append(res.Evidence, fmt.Sprintf("tier=%d 请求失败: %s", tier, clip(err.Error(), 120)))
				if !quiet {
					fmt.Printf("      [%s] tier=%d: 失败 %s\n", model, tier, clip(err.Error(), 70))
				}
				break
			}
			allFailed = false
			// completion_tokens counts the reasoning tokens too on a reasoning
			// model, and max_tokens caps only the visible answer.  Comparing the
			// total against the rung made a request that asked for 2000 and got
			// 2005 content tokens (plus 1214 reasoning) look like a 3219-token
			// ceiling — 60% above anything the model was allowed to emit, and
			// published to the panel as a measurement.
			out := sample.Out
			if sample.HasOut && sample.Reasoning > 0 && sample.Reasoning <= sample.Out {
				out = sample.Out - sample.Reasoning
			}
			rs := ""
			if sample.Reasoning > 0 {
				rs = fmt.Sprintf(" 推理%d", sample.Reasoning)
			}
			if !quiet {
				fmt.Printf("      [%s] tier=%d #%d: 输出 %s tok%s finish=%s %.0fs\n",
					model, tier, attempt, tokenText(out, sample.HasOut), rs, sample.Finish, dt.Seconds())
			}

			if sample.Finish == "length" && sample.HasOut {
				if out < tier {
					// The upstream truncated.  If an earlier rung came back full,
					// the real ceiling sits between that rung and this value, so
					// say so instead of pretending the clamp is exact.
					verdict := "钳制"
					if res.MaxFull > 0 && out < res.MaxFull {
						verdict = fmt.Sprintf("≈%d（钳制；上一档满额 %d，取值 %d）", out, res.MaxFull, out)
					}
					res.Measured = intPtr(out)
					res.Verdict = verdict
					res.Evidence = append(res.Evidence, fmt.Sprintf("要 %d 只给 %d（finish=length）", tier, out))
					res.Elapsed = elapsed()
					return res
				}
				res.Evidence = append(res.Evidence, fmt.Sprintf("tier=%d: 输出满额 %d（支持≥%d）", tier, out, tier))
				res.MaxFull = out
				break
			}

			if sample.Finish == "stop" {
				res.Evidence = append(res.Evidence, fmt.Sprintf("tier=%d #%d: finish=stop（模型主动收尾，输出 %s）",
					tier, attempt, tokenText(out, sample.HasOut)))
				if sample.HasOut && out > res.MaxFull {
					res.MaxFull = out
				}
				if attempt == attempts {
					// A rung that came back full is still evidence: the honest
					// conclusion is "at least this much", not "no conclusion".
					if res.MaxFull > 0 {
						res.Measured = nil
						res.Verdict = fmt.Sprintf("≥%d（模型主动收尾，未测到钳制点；声称 %s）",
							res.MaxFull, claimedText(res.ClaimedOutput))
					} else {
						res.Verdict = "模型主动停止（未能测出上限，非钳制）"
					}
					res.Elapsed = elapsed()
					return res
				}
				continue
			}

			res.Evidence = append(res.Evidence, fmt.Sprintf("tier=%d: 异常返回 out=%s finish=%s",
				tier, tokenText(sample.Out, sample.HasOut), sample.Finish))
			break
		}
	}

	if allFailed {
		// A run that never sent a request because the budget was already spent is
		// not the same as one whose requests all failed; reporting the latter
		// would send the operator hunting for a broken model that was never
		// asked anything.
		if skipped {
			res.Verdict = fmt.Sprintf("未测完（时间预算不足 %ds，未发出请求；已知至少 >%d）", int(budget.Seconds()), res.MaxFull)
		} else {
			res.Verdict = "全部请求失败（模型/账号不可用，无结论）"
		}
		res.Elapsed = elapsed()
		return res
	}
	if res.MaxFull > 0 {
		res.Measured = intPtr(res.MaxFull)
		res.Verdict = fmt.Sprintf("≥%d（最高档位仍满额，未触顶）", res.MaxFull)
	} else if res.Verdict == "" {
		// The ladder answered without concluding: the upstream reported a
		// finish_reason but no token count, so there is no number to publish.
		// Saying so beats the empty verdict this used to leave behind, which
		// printed as a blank 判据 column and read like a tool that had silently
		// given up.
		res.Verdict = "无法判定（上游未返回 token 用量，读数缺失）"
	}
	res.Elapsed = elapsed()
	return res
}

// withTimeout returns a shallow copy of the client with a shorter deadline.  The
// transport is shared, so the copy costs nothing but a struct.
func withTimeout(client *http.Client, d time.Duration) *http.Client {
	cp := *client
	cp.Timeout = d
	return &cp
}

// ---------------------------------------------------------------------------
// Classification into the panel contract
// ---------------------------------------------------------------------------

var floorRe = regexp.MustCompile(`[≥>]\s*(\d+)`)

// classify maps the free-form verdict text onto the three states the panel
// understands, and picks the number to publish beside it.
//
// The panel has no business parsing Chinese prose, so the translation happens
// here: 钳制 / ≈N is "clamped" with a trustworthy measured value, ≥N / 未测完 is
// "at_least" with a known floor, and everything else — the model stopped on its
// own, or every request failed — is "inconclusive" with no number at all.  When
// there is no explicit measurement the floor is recovered from the verdict text,
// because a "≥N" line is exactly as good as a measured value for a lower bound.
func classify(res probeResult) (string, *int) {
	v := res.Verdict
	if strings.HasPrefix(v, "钳制") || strings.HasPrefix(v, "≈") {
		return "clamped", res.Measured
	}
	floor := res.Measured
	if floor == nil && res.MaxFull > 0 {
		floor = intPtr(res.MaxFull)
	}
	if floor == nil {
		if m := floorRe.FindStringSubmatch(v); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				floor = intPtr(n)
			}
		}
	}
	// A floor of zero is "unknown", not "zero tokens": the budget-exhausted
	// verdict always names a bound, and when nothing was measured that bound
	// reads ">0".  Publishing at_least/0 would claim knowledge we do not have.
	if floor != nil && *floor <= 0 {
		floor = nil
	}
	if strings.HasPrefix(v, "≥") || strings.HasPrefix(v, "未测完") {
		if floor != nil {
			return "at_least", floor
		}
		return "inconclusive", nil
	}
	return "inconclusive", nil
}

// probeRecord is one entry of the panel contract.  It is a struct rather than a
// map so the key order is fixed and matches the reference's file, which makes a
// diff between two runs readable.
type probeRecord struct {
	Claimed  *int   `json:"claimed"`
	Measured *int   `json:"measured"`
	Verdict  string `json:"verdict"`
	Note     string `json:"note"`
	TestedAt string `json:"tested_at"`
	Source   string `json:"source"`
}

// writePanelOut merges the run's results into the contract file the panel reads.
//
// Merge, not replace: a model that was not retested keeps the record it already
// had, so re-running the tool for one model narrows the file rather than
// emptying it.  A file that cannot be parsed is rebuilt from scratch instead of
// blocking the new results — a corrupt cache must not cost the operator the run
// they just paid for in tokens.
//
// The write is tmp + rename, so a panel query concurrent with a probe run sees
// either the old file or the new one, never a half-written one.
func writePanelOut(path string, results []probeResult) (int, error) {
	contract := map[string]json.RawMessage{}

	if raw, err := os.ReadFile(path); err == nil {
		var old struct {
			Probes map[string]json.RawMessage `json:"probes"`
		}
		if err := json.Unmarshal(raw, &old); err == nil && old.Probes != nil {
			contract = old.Probes
		}
	}

	for _, r := range results {
		verdict, measured := classify(r)
		rec := probeRecord{
			Claimed:  r.ClaimedOutput,
			Measured: measured,
			Verdict:  verdict,
			Note:     r.Verdict,
			TestedAt: r.Time,
			Source:   probeSource,
		}
		encoded, err := json.Marshal(rec)
		if err != nil {
			return 0, err
		}
		contract[r.Model] = encoded
	}

	out := struct {
		Version int                        `json:"version"`
		Probes  map[string]json.RawMessage `json:"probes"`
	}{Version: 1, Probes: contract}
	blob, err := json.MarshalIndent(out, "", " ")
	if err != nil {
		return 0, err
	}
	blob = append(blob, '\n')

	if dir := dirOf(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return 0, err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o644); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	return len(contract), nil
}

// ---------------------------------------------------------------------------
// Flags and plumbing
// ---------------------------------------------------------------------------

// stringList collects a repeatable flag.  --prefix is the one place more than
// one value is useful, because this gateway namespaces a model by the client
// that serves it (`zcode/glm-5.2`, `workbuddy/glm-5.2`) and the same upstream
// model therefore appears once per client.
//
// The reference defaulted this to `cn:`, the namespace its single-realm
// deployment used.  That default matches nothing here — every id carries a
// client prefix — so a bare run has no sensible default and the tool asks for
// a selector instead of quietly matching zero models.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// matchScore ranks a model id against the --models keywords, mirroring the
// reference: an exact match on a keyword that carries a namespace beats a
// suffix match, which beats a bare substring.  The namespaced form is checked
// first so `cn:glm-5.2` selects exactly that model and not `global:glm-5.2`.
func matchScore(id string, keywords []string) int {
	low := strings.ToLower(id)
	for _, k := range keywords {
		if strings.Contains(k, ":") && low == k {
			return 3
		}
	}
	for _, k := range keywords {
		if !strings.Contains(k, ":") && (low == k || strings.HasSuffix(low, ":"+k)) {
			return 2
		}
	}
	for _, k := range keywords {
		if strings.Contains(low, k) {
			return 1
		}
	}
	return 0
}

// namespaceOf reduces a model id to the prefix a --prefix would select: the
// client name and its slash.  Ids without one are grouped so the listing never
// drops a model.
func namespaceOf(id string) string {
	if i := strings.Index(id, "/"); i >= 0 {
		return id[:i+1]
	}
	return "(无客户端前缀)"
}

// namespaceCounts lists the namespaces a catalogue offers, in stable order, with
// how many models each holds.
func namespaceCounts(all []modelTarget) ([]string, map[string]int) {
	counts := map[string]int{}
	var order []string
	for _, m := range all {
		ns := namespaceOf(m.ID)
		if _, seen := counts[ns]; !seen {
			order = append(order, ns)
		}
		counts[ns]++
	}
	sort.Strings(order)
	return order, counts
}

// namespaceLine renders the client prefixes a catalogue offers, with counts.
func namespaceLine(all []modelTarget) string {
	order, counts := namespaceCounts(all)
	var b strings.Builder
	b.WriteString("本网关的客户端前缀：")
	for i, ns := range order {
		if i > 0 {
			b.WriteString("、")
		}
		fmt.Fprintf(&b, "%s(%d)", ns, counts[ns])
	}
	return b.String()
}

// selectorHelp is what a bare run prints.  It names the namespaces this gateway
// actually serves rather than a generic usage line, because the useful selector
// here is a client name and not a realm.
func selectorHelp(all []modelTarget) string {
	var b strings.Builder
	fmt.Fprintf(&b, "共 %d 个模型可测，但没有指定测哪些。请收窄范围：\n", len(all))
	b.WriteString("  --prefix <客户端>/    例如 --prefix zcode/\n")
	b.WriteString("  --models <子串>       例如 --models glm-5.2（逗号分隔可多个）\n")
	b.WriteString("  --limit N             只测匹配结果的前 N 个\n")
	b.WriteString(namespaceLine(all))
	return b.String()
}

// noMatchHelp says which selector filtered everything out.  "No models matched"
// on its own leaves the operator guessing whether the catalogue is empty or
// their prefix was wrong.
func noMatchHelp(all []modelTarget, models string, prefixes []string) string {
	var b strings.Builder
	if len(prefixes) > 0 {
		fmt.Fprintf(&b, "没有模型匹配 --prefix %s（共 %d 个可测）\n",
			strings.Join(prefixes, ","), len(all))
	} else {
		fmt.Fprintf(&b, "没有模型匹配 --models %q（共 %d 个可测）\n", models, len(all))
	}
	b.WriteString(namespaceLine(all))
	return b.String()
}

func main() {
	var (
		base     = flag.String("base", "", "接口基址，如 http://host:7863/v1（必填）")
		key      = flag.String("key", "", "API Key（也可用环境变量 PROBE_API_KEY）")
		models   = flag.String("models", "", "按子串匹配模型 id（逗号分隔，优先级高于 --prefix）")
		tiersCSV = flag.String("tiers", "40000,100000", "阶梯请求值，逗号分隔（越高越慢越费额度）")
		retries  = flag.Int("retries", 3, "finish=stop 时的重试次数")
		timeout  = flag.Int("timeout", 900, "单次请求超时秒数")
		limit    = flag.Int("limit", 0, "只测前 N 个模型（0=全部）")
		out      = flag.String("out", "probe-max-tokens.jsonl", "结果 JSONL 路径")
		panelOut = flag.String("panel-out", "", "额外写面板契约文件（如 data/output_probes.json；默认关闭）")
		resume   = flag.Bool("resume", false, "跳过已有结果的模型（断点续测）")
		jobs     = flag.Int("jobs", 4, "并行探测的模型数")
		budget   = flag.Int("budget", 600, "单模型时间预算秒数（超出记「未测完」）")
		dryRun   = flag.Bool("dry-run", false, "只列出将要测的模型与阶梯，不发请求")
		quiet    = flag.Bool("quiet", false, "不打印每次尝试明细")
		prefixes stringList
	)
	flag.Var(&prefixes, "prefix", "只测以此前缀开头的模型（可多次；如 zcode/）")
	flag.Parse()

	// Which narrowing flags the operator actually typed.  A bare run on this
	// gateway would fan out over every model every client serves — two rungs
	// each, up to 100k output tokens per rung — so the tool asks for a selector
	// rather than silently spending it (or silently matching nothing).
	given := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { given[f.Name] = true })

	if strings.TrimSpace(*base) == "" {
		fail("需要 --base（接口基址，如 http://host:7863/v1）")
	}
	apiKey := *key
	if apiKey == "" {
		apiKey = os.Getenv("PROBE_API_KEY")
	}
	if apiKey == "" && !*dryRun {
		fail("需要 --key 或环境变量 PROBE_API_KEY")
	}

	tiers := parseTiers(*tiersCSV)
	if len(tiers) == 0 {
		fail("--tiers 至少要有一个数值")
	}
	sort.Ints(tiers)

	client := &http.Client{Timeout: time.Duration(*timeout) * time.Second}

	fmt.Printf("目标: %s\n", *base)
	all, err := fetchModels(client, *base, apiKey)
	if err != nil {
		fail("读取模型列表失败: " + err.Error())
	}

	var targets []modelTarget
	if kws := splitKeywords(*models); len(kws) > 0 {
		best := 0
		for _, m := range all {
			if s := matchScore(m.ID, kws); s > best {
				best = s
			}
		}
		for _, m := range all {
			if best > 0 && matchScore(m.ID, kws) == best {
				targets = append(targets, m)
			}
		}
	} else {
		pfx := []string(prefixes)
		if len(pfx) == 0 {
			// Nothing typed to narrow with.  A dry run is free, so let it list
			// everything; a real run has to be told what to spend on.
			if !given["limit"] && !*dryRun {
				fail(selectorHelp(all))
			}
		}
		for _, m := range all {
			if len(pfx) == 0 {
				targets = append(targets, m)
				continue
			}
			for _, p := range pfx {
				if strings.HasPrefix(m.ID, p) {
					targets = append(targets, m)
					break
				}
			}
		}
	}
	if *limit > 0 && len(targets) > *limit {
		targets = targets[:*limit]
	}
	if len(targets) == 0 {
		fail(noMatchHelp(all, *models, []string(prefixes)))
	}

	// --resume reads back what an earlier run already finished.
	done := map[string]probeResult{}
	if *resume {
		if recs, err := readJSONL(*out); err == nil {
			for _, rec := range recs {
				done[rec.Model] = rec
			}
		}
	}

	fmt.Printf("待测模型 %d 个 | 阶梯 %v | 声称上限来自 /v1/models\n", len(targets), tiers)
	if *dryRun {
		fmt.Println("\n[dry-run] 计划：")
		for _, m := range targets {
			skip := ""
			if _, ok := done[m.ID]; ok {
				skip = "  (已有结果，将跳过)"
			}
			fmt.Printf("  %-44s 声称 %8s%s\n", m.ID, claimedText(m.ClaimedOutput), skip)
		}
		return
	}
	fmt.Println()

	results := make([]probeResult, len(targets))
	var todo []int
	for i, m := range targets {
		if rec, ok := done[m.ID]; ok {
			results[i] = rec
			continue
		}
		todo = append(todo, i)
	}

	// Probing is wall-clock bound — a model emitting 40000 tokens takes minutes —
	// so concurrency is the only real lever on total runtime.  Each result is
	// appended to the JSONL as it lands, under a lock, which is what makes
	// --resume useful after an interrupted run.
	if len(todo) > 0 {
		fmt.Printf("开始探测 %d 个模型（并行 %d，单模型预算 %ds）...\n\n", len(todo), *jobs, *budget)

		var (
			mu       sync.Mutex
			wg       sync.WaitGroup
			finished int
			file     *os.File
		)
		if f, err := os.OpenFile(*out, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			file = f
			defer f.Close()
		} else {
			fmt.Fprintf(os.Stderr, "[!] 无法写入 %s: %v（结果只打印在屏幕上）\n", *out, err)
		}

		slots := make(chan struct{}, max(1, *jobs))
		for _, idx := range todo {
			wg.Add(1)
			slots <- struct{}{}
			// GoSafe, not a bare "go": one bad response must cost that one
			// model, not the whole sweep's results.
			core.GoSafe("probe worker", func(msg string) {
				fmt.Fprintf(os.Stderr, "[!] %s\n", msg)
			}, func() {
				defer wg.Done()
				defer func() { <-slots }()
				m := targets[idx]
				res := probeModel(client, *base, apiKey, m.ID, tiers, *retries, *quiet,
					time.Duration(*budget)*time.Second, m.ClaimedOutput)
				res.ClaimedCtx = m.ClaimedCtx
				res.Time = time.Now().Format("2006-01-02 15:04:05")

				mu.Lock()
				finished++
				n := finished
				results[idx] = res
				if file != nil {
					if blob, err := json.Marshal(res); err == nil {
						file.Write(append(blob, '\n'))
					}
				}
				mu.Unlock()

				fmt.Printf("[%d/%d] %s（声称 %s） → 实测 %s | %s | %ss\n",
					n, len(todo), m.ID, claimedText(m.ClaimedOutput),
					measuredText(res.Measured), res.Verdict, formatElapsed(res.Elapsed))
			})
		}
		wg.Wait()
	}

	printSummary(results)

	// A claimed ceiling higher than the measurement is the exact situation this
	// tool exists to surface, so it gets called out rather than left for the
	// reader to spot in the table.
	var mismatched []probeResult
	for _, r := range results {
		if r.Measured != nil && r.ClaimedOutput != nil && *r.Measured < *r.ClaimedOutput {
			mismatched = append(mismatched, r)
		}
	}
	if len(mismatched) > 0 {
		fmt.Printf("\n⚠ %d 个模型的声称上限高于实测（静默钳制）：\n", len(mismatched))
		for _, r := range mismatched {
			fmt.Printf("    %s: 声称 %d → 实际 %d\n", r.Model, *r.ClaimedOutput, *r.Measured)
		}
	}

	fmt.Printf("\n结果已写入 %s（--resume 可跳过已测模型继续）\n", *out)
	if *panelOut != "" {
		n, err := writePanelOut(*panelOut, results)
		if err != nil {
			fail("写面板探测数据失败: " + err.Error())
		}
		fmt.Printf("面板探测数据已写入 %s（共 %d 条，合并保留未重测模型）\n", *panelOut, n)
	}
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func printSummary(results []probeResult) {
	fmt.Println(strings.Repeat("=", 96))
	fmt.Printf("%-44s %9s %9s  %-28s %6s\n", "模型", "声称", "实测", "判据", "耗时")
	fmt.Println(strings.Repeat("-", 96))
	for _, r := range results {
		verdict := []rune(r.Verdict)
		if len(verdict) > 26 {
			verdict = verdict[:26]
		}
		fmt.Printf("%-44s %9s %9s  %-28s %6s\n",
			r.Model, claimedText(r.ClaimedOutput), measuredText(r.Measured),
			string(verdict), formatElapsed(r.Elapsed))
	}
	fmt.Println(strings.Repeat("=", 96))
}

func readJSONL(path string) ([]probeResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []probeResult
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for scanner.Scan() {
		var rec probeResult
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil || rec.Model == "" {
			continue // a torn last line from an interrupted run is not fatal
		}
		out = append(out, rec)
	}
	return out, scanner.Err()
}

// parseTiers reads the --tiers list.  It sorts here rather than at the call site
// because the ladder's logic depends on climbing: an unsorted list would let a
// later rung be smaller than an earlier one and turn "上一档满额" into nonsense.
func parseTiers(csv string) []int {
	var out []int
	for _, part := range strings.Split(csv, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if n, err := strconv.Atoi(part); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

func splitKeywords(csv string) []string {
	var out []string
	for _, part := range strings.Split(csv, ",") {
		if k := strings.ToLower(strings.TrimSpace(part)); k != "" {
			out = append(out, k)
		}
	}
	return out
}

func dirOf(path string) string {
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		return path[:i]
	}
	return ""
}

func intPtr(n int) *int { return &n }

func claimedText(n *int) string {
	if n == nil {
		return "-"
	}
	return strconv.Itoa(*n)
}

func measuredText(n *int) string {
	if n == nil {
		return "-"
	}
	return strconv.Itoa(*n)
}

func tokenText(n int, ok bool) string {
	if !ok {
		return "-"
	}
	return strconv.Itoa(n)
}

func formatElapsed(seconds float64) string {
	return strconv.FormatFloat(seconds, 'f', -1, 64)
}

// clip shortens a message for a table cell or an evidence line.  It counts runes
// so a Chinese error message is not cut mid-character.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "[!] "+msg)
	os.Exit(1)
}
