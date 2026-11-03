package installer

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// CHANGELOG 的三条机器也依赖的格式约定（issue #148）：
//
//  1. 同一个版本标题只出现一次——发布说明生成器（go-binaries.yml 的 publish
//     job）用 `re.search` 按版本号取段，re.search 天然取**第一个**匹配；重复段
//     是静默的，抽到哪段全凭运气。0.5.0 已经真实发生过：v0.5.1 那笔把一段新的
//     `## [0.5.0]` 追加在旧的上面，此后文件里的段与线上 Release 的正文不再是
//     同一份文本。
//  2. 版本标题严格降序（新的在上）——人靠它一眼判断「最新发的是哪一版」；
//     0.5.47 / 0.5.73 也真实被抬到过文件顶上（0.6.x 之上）。
//  3. 这两条零守卫的原因：已有的 TestChangelogNamesTheInstallerForTheSourceVersion
//     只按**当前**版本号找段，历史段里的任何重复或错序都碰不到它。
//
// 判据是一次遍历的纯文本检查，跑在已有的 go test ./installer/ 里，不需要新依赖。
// ---------------------------------------------------------------------------

var changelogTestHeading = regexp.MustCompile(`(?m)^##[ \t]+\[([^\]]+)\]`)

// semverKey parses an x.y.z version for ordering.  A non-semver label (today
// only "Unreleased") returns nil, and the ordering check skips it: it is the
// documented staging slot, allowed to sit anywhere relative to releases.
func semverKey(label string) []int {
	parts := strings.Split(label, ".")
	if len(parts) != 3 {
		return nil
	}
	out := make([]int, 0, 3)
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 0 {
			return nil
		}
		out = append(out, n)
	}
	return out
}

func TestChangelogVersionsAreUniqueAndDescending(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "CHANGELOG.md"))
	if err != nil {
		t.Fatalf("read CHANGELOG.md: %v", err)
	}

	matches := changelogTestHeading.FindAllSubmatch(body, -1)
	if len(matches) < 3 {
		t.Fatalf("CHANGELOG.md has only %d release headings; the file should carry the release history", len(matches))
	}

	seen := map[string]int{}
	var prev []int
	prevLabel := ""
	for _, m := range matches {
		label := string(m[1])
		if _, dup := seen[label]; dup {
			t.Errorf("CHANGELOG.md 里 `## [%s]` 出现了第二次：发布说明生成器按「第一个匹配」取段，"+
				"重复段会让它静默抽到不是作者想要的那份（issue #148，0.5.0 已真实发生过）", label)
			continue
		}
		seen[label] = 1

		key := semverKey(label)
		if key == nil {
			continue // e.g. [Unreleased]
		}
		if prev != nil {
			// strictly descending: each next heading must be OLDER.
			for i := range key {
				if key[i] != prev[i] {
					if key[i] > prev[i] {
						t.Errorf("CHANGELOG.md 的版本标题不再按「新的在上」排列：`[%s]` 排在了 `[%s]` 之上"+
							"（issue #148；0.5.47/0.5.73 曾被抬到文件顶上）", label, prevLabel)
					}
					break
				}
			}
		}
		prev, prevLabel = key, label
	}
}

// TestChangelogGeneratorFindsTheRightSection pins the consumer, not just the
// file: the publish job extracts a section with "first match wins", so the
// invariant it needs is exactly "one heading per version".  This mirrors the
// workflow's regex and fails on any version whose extraction is ambiguous --
// the same condition the duplicate heading created for v0.5.0.
func TestChangelogGeneratorFindsTheRightSection(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "CHANGELOG.md"))
	if err != nil {
		t.Fatalf("read CHANGELOG.md: %v", err)
	}
	pattern := regexp.MustCompile(`(?m)^##[ \t]+\[([^\]]+)\]`)
	counts := map[string]int{}
	for _, m := range pattern.FindAllSubmatch(body, -1) {
		counts[string(m[1])]++
	}
	// The generator runs for the version being published.  Any version with
	// more than one heading makes its extraction ambiguous; today's test
	// cannot know which versions will be published next, so it demands the
	// invariant across the whole file.
	for label, n := range counts {
		if n > 1 {
			t.Errorf("版本 [%s] 有 %d 个段落：生成器的 re.search 只取第一个，抽到哪份全凭运气（issue #148）", label, n)
		}
	}
}
