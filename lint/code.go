package lint

import (
	"bytes"
	"fmt"

	"github.com/errata-ai/vale/v2/core"
	"github.com/jdkato/regexp"
)

// lintCode lints source code -- whether it be a markup codeblock, a complete
// file, or some other portion of text.
func (l *Linter) lintCode(f *core.File) int {
	var line, match, txt string
	var lnLength, padding int
	var block bytes.Buffer

	lines := 0
	comments := core.CommentsByNormedExt[f.NormedExt]
	if len(comments) == 0 {
		return lines
	}

	scope := "%s" + f.RealExt
	inline := regexp.MustCompile(comments["inline"])
	blockStart := regexp.MustCompile(comments["blockStart"])
	blockEnd := regexp.MustCompile(comments["blockEnd"])
	ignore := false
	inBlock := false

	for f.Scanner.Scan() {
		line = core.Sanitize(f.Scanner.Text() + "\n")
		lnLength = len(line)
		lines++
		if inBlock {
			// We're in a block comment.
			if match = blockEnd.FindString(line); len(match) > 0 {
				// We've found the end of the block.
				block.WriteString(line)
				txt = block.String()
				b := core.NewBlock(
					txt, txt, fmt.Sprintf(scope, "text.comment.block"))
				l.lintText(f, b, 0)
				block.Reset()
				inBlock = false
			} else {
				block.WriteString(line)
			}
		} else if match = inline.FindString(line); len(match) > 0 {
			// We've found an inline comment. We need padding here in order to
			// calculate the column span because, for example, a line like
			// 'print("foo") # ...' will be condensed to '# ...'.
			padding = lnLength - len(match)
			b := core.NewBlock(
				match, match, fmt.Sprintf(scope, "text.comment.line"))
			l.lintText(f, b, padding-1)
		} else if match = blockStart.FindString(line); len(match) > 0 && !ignore {
			// We've found the start of a block comment.
			block.WriteString(line)
			inBlock = true
		} else if match = blockEnd.FindString(line); len(match) > 0 {
			ignore = !ignore
		}
	}
	return lines
}
