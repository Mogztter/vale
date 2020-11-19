package lint

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"github.com/errata-ai/vale/v2/core"
	"golang.org/x/net/html"
)

// skipTags are tags that we don't want to lint.
var skipTags = []string{"script", "style", "pre", "figure"}

// skipClasses are classes that we don't want to lint:
// 	- `problematic` is added by rst2html to processing errors which, in our
// 	  case, could be things like file-insertion URLs.
// 	- `pre` is added by rst2html to code spans.
var skipClasses = []string{"problematic", "pre"}
var inlineTags = []string{
	"b", "big", "i", "small", "abbr", "acronym", "cite", "dfn", "em", "kbd",
	"strong", "a", "br", "img", "span", "sub", "sup", "code", "tt", "del"}
var tagToScope = map[string]string{
	"th":         "text.table.header",
	"td":         "text.table.cell",
	"li":         "text.list",
	"blockquote": "text.blockquote",

	// NOTE: These shouldn't inherit from `text`
	// (or else they'll be linted twice.)
	"strong": "strong",
	"b":      "strong",
	"a":      "link",
	"em":     "emphasis",
	"i":      "emphasis",
	"code":   "code",
}

func (l Linter) lintHTMLTokens(f *core.File, raw []byte, offset int) {
	var attr string
	var inBlock, inline, skip, skipClass bool

	buf := bytes.NewBufferString("")

	// The user has specified a custom list of tags/classes to ignore.
	if len(l.Manager.Config.SkippedScopes) > 0 {
		skipTags = l.Manager.Config.SkippedScopes
	}
	if len(l.Manager.Config.IgnoredClasses) > 0 {
		skipClasses = append(skipClasses, l.Manager.Config.IgnoredClasses...)
	}

	skipped := []string{"tt", "code"}
	if len(l.Manager.Config.IgnoredScopes) > 0 {
		skipped = l.Manager.Config.IgnoredScopes
	}

	walker := newWalker(f, raw, offset)
	for {
		tokt, tok, txt := walker.walk()
		skipClass = checkClasses(attr, skipClasses)
		if tokt == html.ErrorToken {
			break
		} else if tokt == html.StartTagToken && core.StringInSlice(txt, skipTags) {
			inBlock = true
		} else if inBlock && core.StringInSlice(txt, skipTags) {
			inBlock = false
		} else if tokt == html.StartTagToken {
			inline = core.StringInSlice(txt, inlineTags)
			skip = core.StringInSlice(txt, skipped)
			walker.addTag(txt)
		} else if tokt == html.EndTagToken && core.StringInSlice(txt, inlineTags) {
			walker.activeTag = ""
		} else if tokt == html.CommentToken {
			f.UpdateComments(txt)
		} else if tokt == html.TextToken {
			skip = skip || shouldBeSkipped(walker.tagHistory, f.NormedExt)
			if scope, match := tagToScope[walker.activeTag]; match {
				if core.StringInSlice(walker.activeTag, inlineTags) {
					// NOTE: We need to create a "temporary" context because
					// this text is actually linted twice: once as a 'link' and
					// once as part of the overall paragraph. See issue #105
					// for more info.
					tempCtx := updateContext(walker.context, walker.queue)
					l.lintText(f, core.NewBlock(tempCtx, txt, scope), 0)
					walker.activeTag = ""
				}
			}
			walker.append(txt)
			if !inBlock && txt != "" {
				txt, skip = clean(txt, f.NormedExt, skip, skipClass, inline)
				buf.WriteString(txt)
			}
		}

		if tokt == html.EndTagToken && !core.StringInSlice(txt, inlineTags) {
			content := buf.String()
			if strings.TrimSpace(content) != "" {
				l.lintScope(f, walker, content)
			}
			walker.reset()
			buf.Reset()
		}

		attr = getAttribute(tok, "class")

		walker.replaceToks(tok)
		l.lintTags(f, walker, tok)
	}

	// `scope: summary`
	l.lintText(
		f,
		core.NewBlock(f.Content, f.Summary.String(), "summary."+f.RealExt),
		0)

	// `scope: raw`
	l.lintText(f, core.NewBlock("", f.Content, "raw."+f.RealExt), 0)
}

func (l Linter) lintScope(f *core.File, state walker, txt string) {
	for _, tag := range state.tagHistory {
		scope, match := tagToScope[tag]
		if (match && !core.StringInSlice(tag, inlineTags)) || heading.MatchString(tag) {
			if match {
				scope = scope + f.RealExt
			} else {
				scope = "text.heading." + tag + f.RealExt
			}
			txt = strings.TrimLeft(txt, " ")
			b := state.block(txt, scope)
			l.lintText(f, b, 0)
			return
		}
	}

	// NOTE: We don't include headings, list items, or table cells (which are
	// processed above) in our Summary content.
	f.Summary.WriteString(txt + " ")

	b := state.block(txt, "txt")
	l.lintProse(f, b, 0)
}

func (l Linter) lintTags(f *core.File, state walker, tok html.Token) {
	if tok.Data == "img" {
		for _, a := range tok.Attr {
			if a.Key == "alt" {
				l.lintText(
					f,
					state.block(a.Val, "text.attr."+a.Key), 0)
			}
		}
	}
}

func checkClasses(attr string, ignore []string) bool {
	for _, class := range strings.Split(attr, " ") {
		if core.StringInSlice(class, ignore) {
			return true
		}
	}
	return false
}

// HACK: We need to look for inserted `spans` within `tt` tags.
//
// See https://github.com/errata-ai/vale/v2/issues/140.
func shouldBeSkipped(tagHistory []string, ext string) bool {
	if ext == ".rst" {
		n := len(tagHistory)
		for i := n - 1; i >= 0; i-- {
			if tagHistory[i] == "span" {
				continue
			}
			return tagHistory[i] == "tt" && i+1 != n
		}
	}
	return false
}

func codify(ext, text string) string {
	if ext == ".md" || ext == ".adoc" {
		return "`" + text + "`"
	} else if ext == ".rst" {
		return "``" + text + "``"
	}
	return text
}

func clean(txt, ext string, skip, skipClass, inline bool) (string, bool) {
	punct := []string{".", "?", "!", ",", ":", ";"}
	first, _ := utf8.DecodeRuneInString(txt)
	starter := core.StringInSlice(string(first), punct) && !skip
	if skip || skipClass {
		txt, _ = core.Substitute(txt, txt, '*')
		txt = codify(ext, txt)
		skip = false
	}
	if inline && !starter {
		txt = " " + txt
	}
	return txt, skip
}

func getAttribute(tok html.Token, key string) string {
	for _, attr := range tok.Attr {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}

func updateContext(ctx string, queue []string) string {
	for _, s := range queue {
		ctx = updateCtx(ctx, s, html.TextToken)
	}
	return ctx
}

func clearElements(ctx string, tok html.Token) string {
	if tok.Data == "img" || tok.Data == "a" || tok.Data == "p" || tok.Data == "script" {
		for _, a := range tok.Attr {
			if a.Key == "href" || a.Key == "id" || a.Key == "src" {
				ctx = updateCtx(ctx, a.Val, html.TextToken)
			}
		}
	}
	return ctx
}

func updateCtx(ctx, txt string, tokt html.TokenType) string {
	var found bool
	if (tokt == html.TextToken || tokt == html.CommentToken) && txt != "" {
		for _, s := range strings.Split(txt, "\n") {
			ctx, found = core.Substitute(ctx, s, '@')
			if !found {
				for _, w := range strings.Fields(s) {
					ctx, _ = core.Substitute(ctx, w, '@')
				}
			}
		}
	}
	return ctx
}
