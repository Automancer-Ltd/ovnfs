package checkout

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"strings"

	"github.com/stencil-hq/ovnfs/store"
)

// gitPatch appends one path's change in git's patch format: a diff --git
// header, the modes of a file added, deleted or changing mode, and text hunks.
// It returns the lines those hunks add and delete, or that the content is
// binary. A zero side is absent. A change between a symlink and a file is a
// deletion and an addition.
//
// With full set the patch is the one git diff --binary writes, which git
// apply takes whole: a and b carry git blob ids for the index lines, and
// binary content travels as base85 literals. Without it no blob id is
// written and a binary file is only named, as git diff does, so git apply
// refuses that file instead of skipping it.
func gitPatch(w *strings.Builder, p string, a, b store.File, old, cur []byte, full bool) (added, deleted int, binary bool) {
	if a.ID != "" && b.ID != "" && (a.Mode == store.ModeSymlink) != (b.Mode == store.ModeSymlink) {
		_, deleted, gone := gitPatch(w, p, a, store.File{}, old, nil, full)
		added, _, binary = gitPatch(w, p, store.File{}, b, nil, cur, full)
		return added, deleted, binary || gone
	}
	fmt.Fprintf(w, "diff --git %s %s\n", quotePath("a/"+p), quotePath("b/"+p))
	oldID, newID := zeroOID, zeroOID
	if a.ID != "" {
		oldID = string(a.ID)
	}
	if b.ID != "" {
		newID = string(b.ID)
	}
	index := func(mode string) {
		if full {
			fmt.Fprintf(w, "index %s..%s%s\n", oldID, newID, mode)
		}
	}
	labelA, labelB := quotePath("a/"+p), quotePath("b/"+p)
	switch {
	case a.ID == "":
		fmt.Fprintf(w, "new file mode %06o\n", b.Mode)
		index("")
		labelA = "/dev/null"
	case b.ID == "":
		fmt.Fprintf(w, "deleted file mode %06o\n", a.Mode)
		index("")
		labelB = "/dev/null"
	case a.Mode != b.Mode:
		fmt.Fprintf(w, "old mode %06o\nnew mode %06o\n", a.Mode, b.Mode)
		if a.ID == b.ID {
			return 0, 0, false
		}
		index("")
	default:
		index(fmt.Sprintf(" %06o", a.Mode))
	}
	if binaryContent(old, cur) {
		if full {
			w.WriteString("GIT binary patch\n")
			binaryLiteral(w, cur)
			binaryLiteral(w, old)
		} else {
			fmt.Fprintf(w, "Binary files %s and %s differ\n", labelA, labelB)
		}
		return 0, 0, true
	}
	added, deleted = unifiedHunks(w, labelA, labelB, old, cur)
	return added, deleted, false
}

// zeroOID stands for an absent side in an index line.
const zeroOID = "0000000000000000000000000000000000000000"

// unifiedHunks appends the ---/+++ header and hunks turning old into cur, nothing
// when they are equal, and returns the lines they add and delete.
func unifiedHunks(w *strings.Builder, labelA, labelB string, old, cur []byte) (added, deleted int) {
	a, b := splitLines(old), splitLines(cur)
	script := lineDiff(a, b)
	header := false
	for i := 0; i < len(script); {
		if script[i] == opEqual {
			i++
			continue
		}
		// A hunk spans changes separated by at most 2*diffContext equal lines,
		// padded with diffContext equal lines on either side.
		start := max(0, i-diffContext)
		end := i
		for end < len(script) {
			if script[end] != opEqual {
				end++
				continue
			}
			run := end
			for run < len(script) && script[run] == opEqual {
				run++
			}
			if run == len(script) || run-end > 2*diffContext {
				end = min(run, end+diffContext)
				break
			}
			end = run
		}
		if !header {
			fmt.Fprintf(w, "--- %s\n+++ %s\n", labelA, labelB)
			header = true
		}
		writeHunk(w, script, start, end, a, b)
		i = end
	}
	return countEdits(script)
}

// binaryLiteral appends a git binary patch hunk holding data whole: "literal",
// its length, then its zlib-compressed bytes in base85 lines of at most 52 bytes,
// each prefixed by its length ('A'-'Z' for 1-26, 'a'-'z' for 27-52), and a blank line.
func binaryLiteral(w *strings.Builder, data []byte) {
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	zw.Write(data)
	zw.Close()
	fmt.Fprintf(w, "literal %d\n", len(data))
	deflated := z.Bytes()
	for len(deflated) > 0 {
		n := min(len(deflated), 52)
		if n <= 26 {
			w.WriteByte(byte('A' + n - 1))
		} else {
			w.WriteByte(byte('a' + n - 27))
		}
		base85(w, deflated[:n])
		w.WriteByte('\n')
		deflated = deflated[n:]
	}
	w.WriteByte('\n')
}

// base85Alphabet is git's base85 digit set.
const base85Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz!#$%&()*+-;<=>?@^_`{|}~"

// base85 appends data as git encodes it: each group of four bytes, the last padded
// with zeros, as five digits, most significant first.
func base85(w *strings.Builder, data []byte) {
	for len(data) > 0 {
		var v uint32
		for i := range 4 {
			v <<= 8
			if i < len(data) {
				v |= uint32(data[i])
			}
		}
		var digits [5]byte
		for i := 4; i >= 0; i-- {
			digits[i] = base85Alphabet[v%85]
			v /= 85
		}
		w.Write(digits[:])
		data = data[min(len(data), 4):]
	}
}

// quotePath spells a patch path as git does: as is unless it holds a double quote,
// a backslash or a control character, then C-quoted. Bytes above ASCII stay raw,
// as with core.quotePath off; git apply reads both.
func quotePath(p string) string {
	if !strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '"' || r == '\\' }) {
		return p
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := range len(p) {
		switch c := p[i]; c {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '\a':
			b.WriteString(`\a`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\v':
			b.WriteString(`\v`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if c < 0x20 || c == 0x7f {
				fmt.Fprintf(&b, `\%03o`, c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
