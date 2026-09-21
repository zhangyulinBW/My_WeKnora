package aichat

// 本文件是 internal/handler/session/artifact_reference.go 的包内私有镜像，
// 供 /api/v1/ai/chat 协议在智能体回合结束后处理沙箱产物时复用同一套语义：
// 把答案里模型写下的沙箱本地文件引用（`![描述](sandbox:文件名)` 等写法）
// 归一化为能在沙箱回收后继续解析的稳定引用形式 `resource://<handle>`。
//
// 官方 /agent-chat 流在其完成路径（session.AgentStreamHandler.handleComplete）
// 里做同样的重写；ai/chat 是独立的协议适配层，按要求不改动 session 包，
// 因此在此镜像实现。两处逻辑需保持同步，修改时请对照另一份。
//
// 无资源目录（resource catalog）的部署没有稳定句柄可用，此时引用被归一化
// 为规范的 `sandbox:<文件名>` 写法，由客户端对照消息自身的 artifacts 列表
// 按名解析——只在会话内可读。

import (
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

// fencedOrInlineCodeRE splits content so destinations inside code samples are
// never rewritten — a documentation snippet showing the syntax must survive.
var fencedOrInlineCodeRE = regexp.MustCompile("(?s)(```.*?```|~~~.*?~~~|`[^`\n]*`)")

// schemeRE matches an already-qualified URL (http://, resource://, data:, …).
var schemeRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.\-]*:`)

// titleSuffixRE splits the optional title off a link destination
// (`file.png "caption"`).
var titleSuffixRE = regexp.MustCompile(`(?s)^(.*?)(\s+(?:"[^"]*"|'[^']*'))$`)

// rewriteArtifactReferences replaces every Markdown link/image destination that
// names one of the turn's artifacts with that artifact's stable reference.
//
// Three destination spellings are accepted, because models are inconsistent
// about the prefix even when the prompt asks for one:
//
//	![评分](sandbox:市场画像评分.html)
//	![评分](市场画像评分.html)
//	![评分](./output/市场画像评分.html)
//
// Anything else — an http URL, a resource:// handle the model copied from
// context, an unknown file name — is returned unchanged so existing behaviour
// (knowledge-base images, web links) is untouched.
func rewriteArtifactReferences(content string, artifacts types.MessageArtifacts) string {
	if content == "" || len(artifacts) == 0 {
		return content
	}
	byName := artifactRefByName(artifacts)
	if len(byName) == 0 {
		return content
	}

	parts := fencedOrInlineCodeRE.Split(content, -1)
	if len(parts) == 1 {
		return rewriteArtifactReferencesInSegment(content, byName)
	}
	code := fencedOrInlineCodeRE.FindAllString(content, -1)
	var out strings.Builder
	out.Grow(len(content))
	for i, part := range parts {
		out.WriteString(rewriteArtifactReferencesInSegment(part, byName))
		if i < len(code) {
			out.WriteString(code[i])
		}
	}
	return out.String()
}

// rewriteArtifactReferencesInSegment walks every inline Markdown link/image in
// one non-code segment and rebinds the ones that name an artifact.
//
// Destinations are located by matching parentheses rather than by regex,
// because skill-generated file names routinely contain both spaces and
// parentheses (`腾讯控股(00700) 成交量_838ccc.html`). A regex that stops at the
// first space or paren would capture half the name and never match.
func rewriteArtifactReferencesInSegment(segment string, byName map[string]string) string {
	if segment == "" || !strings.Contains(segment, "](") {
		return segment
	}

	var out strings.Builder
	out.Grow(len(segment))
	cursor := 0
	for cursor < len(segment) {
		relative := strings.Index(segment[cursor:], "](")
		if relative < 0 {
			break
		}
		closeBracket := cursor + relative
		open := closeBracket + 1

		inner, end, ok := scanLinkDestination(segment, open)
		if !ok || !hasLinkLabelBefore(segment, closeBracket) {
			out.WriteString(segment[cursor : open+1])
			cursor = open + 1
			continue
		}

		destination, title := splitDestinationTitle(inner)
		ref, matched := lookupArtifactRef(destination, byName, markdownImageBefore(segment, closeBracket))
		if !matched {
			out.WriteString(segment[cursor : end+1])
			cursor = end + 1
			continue
		}

		out.WriteString(segment[cursor : open+1])
		out.WriteString(ref)
		out.WriteString(title)
		out.WriteString(")")
		cursor = end + 1
	}
	out.WriteString(segment[cursor:])
	return out.String()
}

// scanLinkDestination returns the text between the `(` at openIndex and its
// matching `)`, plus the index of that closing paren. A destination never spans
// a line break, so a newline ends the scan unsuccessfully.
func scanLinkDestination(text string, openIndex int) (string, int, bool) {
	depth := 1
	for i := openIndex + 1; i < len(text); i++ {
		switch text[i] {
		case '\n':
			return "", 0, false
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return text[openIndex+1 : i], i, true
			}
		}
	}
	return "", 0, false
}

// hasLinkLabelBefore reports whether `](` is preceded by a `[` on the same
// line, i.e. whether this really is a link rather than incidental punctuation.
func hasLinkLabelBefore(text string, closeBracketIndex int) bool {
	for i := closeBracketIndex - 1; i >= 0; i-- {
		switch text[i] {
		case '\n':
			return false
		case '[':
			return true
		}
	}
	return false
}

// splitDestinationTitle separates `dest "title"` into its two parts, returning
// the title with its leading whitespace so it can be re-emitted verbatim.
func splitDestinationTitle(inner string) (string, string) {
	if groups := titleSuffixRE.FindStringSubmatch(inner); groups != nil {
		return strings.TrimSpace(groups[1]), groups[2]
	}
	return strings.TrimSpace(inner), ""
}

func markdownImageBefore(text string, closeBracketIndex int) bool {
	for i := closeBracketIndex - 1; i >= 0; i-- {
		switch text[i] {
		case '\n':
			return false
		case '[':
			return i > 0 && text[i-1] == '!'
		}
	}
	return false
}

func looksLikeSandboxOutputPath(candidate string) bool {
	c := strings.TrimPrefix(strings.TrimSpace(candidate), "./")
	c = strings.TrimPrefix(c, "/")
	return strings.HasPrefix(c, "workspace/output/") || strings.HasPrefix(c, "output/")
}

// lookupArtifactRef resolves one Markdown destination to an artifact reference.
func lookupArtifactRef(destination string, byName map[string]string, image bool) (string, bool) {
	name, ok := artifactDestinationName(destination, image)
	if !ok {
		return "", false
	}
	ref, ok := byName[name]
	return ref, ok
}

// artifactDestinationName normalizes one Markdown link/image destination down to
// the sandbox-output file name it names. ok is false when the destination is not
// a sandbox-local file name at all — a real URL, a resource:// handle, a bare
// name in an ordinary link, or incidental prose punctuation.
func artifactDestinationName(destination string, image bool) (string, bool) {
	candidate := strings.TrimSpace(destination)
	if candidate == "" {
		return "", false
	}
	// Trim the optional angle-bracket form Markdown allows around a
	// destination: `[x](<name with space>)` — kept for completeness even
	// though the outer regex rejects inner whitespace.
	candidate = strings.TrimPrefix(strings.TrimSuffix(candidate, ">"), "<")

	hadSandboxPrefix := false
	for _, prefix := range []string{"sandbox://", "sandbox:"} {
		if len(candidate) >= len(prefix) && strings.EqualFold(candidate[:len(prefix)], prefix) {
			candidate = candidate[len(prefix):]
			hadSandboxPrefix = true
			break
		}
	}
	// A destination that already carries a scheme is a real URL, not a file
	// name. `sandbox:` is the one exception and was stripped above.
	if !hadSandboxPrefix && schemeRE.MatchString(candidate) {
		return "", false
	}

	// Models percent-encode non-ASCII names about half the time.
	if decoded, err := url.PathUnescape(candidate); err == nil {
		candidate = decoded
	}
	candidate = strings.TrimSpace(candidate)
	// Bare names are rewritten for images (`![chart](a.html)`) because that is
	// how models actually cite generated files. Ordinary links (`[docs](README.md)`)
	// are left alone unless they already carry a sandbox prefix or path —
	// otherwise a colliding artifact name would hijack a real hyperlink.
	if !hadSandboxPrefix && !image && !looksLikeSandboxOutputPath(candidate) {
		return "", false
	}
	// Directory prefixes (`./`, `output/`, `/workspace/output/`) carry no
	// information the artifact list does not already have.
	candidate = path.Base(candidate)
	if candidate == "" || candidate == "." || candidate == "/" {
		return "", false
	}
	return candidate, true
}

// forEachArtifactDestination calls fn with the normalized file name of every
// link/image destination in content. Destinations inside code spans or fences
// are skipped, mirroring rewriteArtifactReferences.
func forEachArtifactDestination(content string, fn func(name string)) {
	if content == "" || !strings.Contains(content, "](") {
		return
	}
	// Split returns only the non-code segments (Go does not interleave the
	// matched fences the way a capturing-group Python split would). Walk
	// every part; skipping odd indexes would drop citations after the first
	// fence, which rewriteArtifactReferences still rewrites.
	for _, part := range fencedOrInlineCodeRE.Split(content, -1) {
		walkSegmentDestinations(part, fn)
	}
}

func walkSegmentDestinations(segment string, fn func(name string)) {
	cursor := 0
	for cursor < len(segment) {
		relative := strings.Index(segment[cursor:], "](")
		if relative < 0 {
			return
		}
		closeBracket := cursor + relative
		open := closeBracket + 1

		inner, end, ok := scanLinkDestination(segment, open)
		if !ok || !hasLinkLabelBefore(segment, closeBracket) {
			cursor = open + 1
			continue
		}
		destination, _ := splitDestinationTitle(inner)
		if name, ok := artifactDestinationName(destination, markdownImageBefore(segment, closeBracket)); ok {
			fn(name)
		}
		cursor = end + 1
	}
}

// referencedArtifacts returns the candidates the answer body names, in
// candidate order.
//
// A candidate is named when the body carries its resource:// handle, or when a
// link/image destination resolves to its file name — the same spellings
// rewriteArtifactReferences accepts. Matching goes through the Markdown
// destination rules rather than a substring search, so a file that merely
// appears in prose is not treated as a reference.
//
// Candidates are matched in the order given: listing this turn's artifacts
// first lets a regenerated file shadow the older version of itself, so only an
// explicit handle reference still pulls the old version in.
func referencedArtifacts(content string, candidates types.MessageArtifacts) types.MessageArtifacts {
	if content == "" || len(candidates) == 0 {
		return nil
	}

	matched := make([]bool, len(candidates))
	for _, handle := range types.ScanResourceReferences(content) {
		for i := range candidates {
			if candidates[i].URL == handle {
				matched[i] = true
				break
			}
		}
	}

	nameToIndex := make(map[string]int, len(candidates))
	for i := range candidates {
		name := strings.TrimSpace(candidates[i].FileName)
		if name == "" {
			continue
		}
		if _, exists := nameToIndex[name]; exists {
			continue
		}
		nameToIndex[name] = i
	}
	if len(nameToIndex) > 0 {
		forEachArtifactDestination(content, func(name string) {
			if i, ok := nameToIndex[name]; ok {
				matched[i] = true
			}
		})
	}

	var out types.MessageArtifacts
	for i := range candidates {
		if matched[i] {
			out = append(out, candidates[i])
		}
	}
	return out
}

// artifactRefByName maps each artifact's file name to the reference that
// replaces it in the answer. A duplicate name keeps the first occurrence:
// silently preferring the later file would make the reference point at
// something the model did not describe.
func artifactRefByName(artifacts types.MessageArtifacts) map[string]string {
	byName := make(map[string]string, len(artifacts))
	for _, artifact := range artifacts {
		name := strings.TrimSpace(artifact.FileName)
		if name == "" {
			continue
		}
		if _, exists := byName[name]; exists {
			continue
		}
		byName[name] = artifactReference(artifact)
	}
	return byName
}

// artifactReference returns the destination an answer should carry for one
// artifact: its catalog handle when the deployment has a resource registry,
// otherwise the canonical chat-only `sandbox:<name>` form.
//
// The raw storage path is never a candidate. It names a bucket and key, which
// is both an information leak and useless to a client that has no credentials
// for the object store.
func artifactReference(artifact types.MessageArtifact) string {
	if handle, ok := types.ParseResourcePath(artifact.URL); ok {
		return types.BuildResourcePath(handle)
	}
	return "sandbox:" + strings.TrimSpace(artifact.FileName)
}

// artifactsNewestFirst returns a copy of artifacts in reverse order so the
// latest recorded version of a name is first. KnownArtifacts is creation
// order (oldest first); a hash-skipped citation must bind the fork-point
// file, not the first time that name appeared. A copy is required: reversing
// in place would mutate the store's backing slice.
func artifactsNewestFirst(artifacts types.MessageArtifacts) types.MessageArtifacts {
	n := len(artifacts)
	if n < 2 {
		return artifacts
	}
	out := make(types.MessageArtifacts, n)
	for i, artifact := range artifacts {
		out[n-1-i] = artifact
	}
	return out
}

// mergeArtifactLists concatenates artifact lists in order, keeping the first
// occurrence of each storage URL. Order is preserved because a reference
// resolves to an index into the resulting list, so the caller's ordering
// decides which version a name points at: putting this turn's artifacts first
// makes a regenerated file shadow the older one.
func mergeArtifactLists(lists ...types.MessageArtifacts) types.MessageArtifacts {
	var out types.MessageArtifacts
	var seen map[string]struct{}
	for _, list := range lists {
		for _, artifact := range list {
			key := strings.TrimSpace(artifact.URL)
			if key != "" {
				if _, exists := seen[key]; exists {
					continue
				}
				if seen == nil {
					seen = make(map[string]struct{})
				}
				seen[key] = struct{}{}
			}
			out = append(out, artifact)
		}
	}
	return out
}

// historyOnlyArtifacts returns the entries of referenced that this turn did not
// produce. Those are the ones whose ownership has to be extended to the new
// message; the collector already bound the ones it persisted itself.
func historyOnlyArtifacts(referenced, current types.MessageArtifacts) types.MessageArtifacts {
	if len(referenced) == 0 {
		return nil
	}
	produced := make(map[string]struct{}, len(current))
	for _, artifact := range current {
		produced[strings.TrimSpace(artifact.URL)] = struct{}{}
	}
	var out types.MessageArtifacts
	for _, artifact := range referenced {
		if _, isCurrent := produced[strings.TrimSpace(artifact.URL)]; isCurrent {
			continue
		}
		out = append(out, artifact)
	}
	return out
}

// artifactHandle returns the artifact's `resource://<handle>` reference, or ""
// when the deployment stores artifacts without a resource catalog.
func artifactHandle(artifact types.MessageArtifact) string {
	if handle, ok := types.ParseResourcePath(artifact.URL); ok {
		return types.BuildResourcePath(handle)
	}
	return ""
}

// publicArtifactViews returns a redacted view of the artifact list suitable
// for direct serialization onto the SSE stream. The physical storage path is
// stripped; the resource handle is kept because it is what the answer body
// references, and clients need it to tie an inline reference to the file it
// names. Bytes are fetched through the download endpoint that enforces tenant
// ownership.
func publicArtifactViews(list types.MessageArtifacts) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(list))
	for i, a := range list {
		out = append(out, map[string]interface{}{
			"index":       i,
			"handle":      artifactHandle(a),
			"file_name":   a.FileName,
			"file_type":   a.FileType,
			"file_size":   a.FileSize,
			"source_path": a.SourcePath,
			"mod_time":    a.ModTime,
			"created_at":  a.CreatedAt,
		})
	}
	return out
}
