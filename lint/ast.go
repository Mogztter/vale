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

func (l Linter) lintHTMLTokens(f *core.File, ctx string, fsrc []byte, offset int) {
	var txt, attr, tag string
	var tokt html.TokenType
	var tok html.Token
	var inBlock, inline, skip, skipClass bool

	lines := len(f.Lines) + offset
	buf := bytes.NewBufferString("")

	// The user has specified a custom list of tags/classes to ignore.
	if len(l.Manager.Config.SkippedScopes) > 0 {
		skipTags = l.Manager.Config.SkippedScopes
	}
	if len(l.Manager.Config.IgnoredClasses) > 0 {
		skipClasses = append(skipClasses, l.Manager.Config.IgnoredClasses...)
	}

	// queue holds each segment of text we encounter in a block, which we then
	// use to sequentially update our context.
	queue := []string{}

	// tagHistory holds the HTML tags we encounter in a given block -- e.g.,
	// if we see <ul>, <li>, <p>, we'd get tagHistory = [ul li p]. It's reset
	// on every non-inline end tag.
	tagHistory := []string{}

	tokens := html.NewTokenizer(bytes.NewReader(fsrc))

	skipped := []string{"tt", "code"}

	walker := NewWalker(ctx, offset)
	if len(l.Manager.Config.IgnoredScopes) > 0 {
		skipped = l.Manager.Config.IgnoredScopes
	}

	for {
		tokt = tokens.Next()
		tok = tokens.Token()
		txt = html.UnescapeString(strings.TrimSpace(tok.Data))

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
			tagHistory = append(tagHistory, txt)
			tag = txt
		} else if tokt == html.EndTagToken && core.StringInSlice(txt, inlineTags) {
			tag = ""
		} else if tokt == html.CommentToken {
			f.UpdateComments(txt)
		} else if tokt == html.TextToken {
			skip = skip || shouldBeSkipped(tagHistory, f.NormedExt)
			if scope, match := tagToScope[tag]; match && core.StringInSlice(tag, inlineTags) {
				// NOTE: We need to create a "temporary" context because this
				// text is actually linted twice: once as a 'link' and once as
				// part of the overall paragraph. See issue #105 for more info.
				tempCtx := updateContext(ctx, queue)
				l.lintText(f, core.NewBlock(tempCtx, txt, scope), lines, 0)
				tag = ""
			}
			queue = append(queue, txt)
			if !inBlock && txt != "" {
				txt, skip = clean(txt, f.NormedExt, skip, skipClass, inline)
				buf.WriteString(txt)
			}
		}

		if tokt == html.EndTagToken && !core.StringInSlice(txt, inlineTags) {
			content := buf.String()
			if strings.TrimSpace(content) != "" {
				l.lintScope(f, ctx, content, tagHistory, lines)
			}

			ctx = updateContext(ctx, queue)
			queue = []string{}
			tagHistory = []string{}

			buf.Reset()
		}

		attr = getAttribute(tok, "class")
		ctx = clearElements(ctx, tok)

		if tok.Data == "img" {
			for _, a := range tok.Attr {
				if a.Key == "alt" {
					block := core.NewBlock(ctx, a.Val, "text.attr."+a.Key)
					l.lintText(f, block, lines, 0)
				}
			}
		}
	}

	summary := core.NewBlock(f.Content, f.Summary.String(), "summary."+f.RealExt)
	l.lintText(f, summary, lines, 0)

	// Run all rules with `scope: raw`
	l.lintText(f, core.NewBlock("", f.Content, "raw."+f.RealExt), lines, 0)
}

func (l Linter) lintScope(f *core.File, ctx, txt string, tags []string, lines int) {
	for _, tag := range tags {
		scope, match := tagToScope[tag]
		if (match && !core.StringInSlice(tag, inlineTags)) || heading.MatchString(tag) {
			if match {
				scope = scope + f.RealExt
			} else {
				scope = "text.heading." + tag + f.RealExt
			}
			txt = strings.TrimLeft(txt, " ")
			l.lintText(f, core.NewBlock(ctx, txt, scope), lines, 0)
			return
		}
	}

	// NOTE: We don't include headings, list items, or table cells (which are
	// processed above) in our Summary content.
	f.Summary.WriteString(txt + " ")
	l.lintProse(f, ctx, txt, lines, 0)
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
