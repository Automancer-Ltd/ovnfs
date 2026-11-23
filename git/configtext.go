package git

import (
	"iter"
	"slices"
	"strings"
)

// configBOM is the UTF-8 byte order mark git skips at the very start of a config.
const configBOM = "\xef\xbb\xbf"

// configItemKind tells what a configItem is.
type configItemKind uint8

const (
	// configHeader is a section header, "[name]" or "[name \"sub\"]".
	configHeader configItemKind = iota
	// configVariable is a variable: its name through its last continued line.
	configVariable
	// configComment is a comment line, or a line git would reject.
	configComment
)

// configItem is one token of git config text. Offsets index the scanned text.
type configItem struct {
	kind configItemKind
	// at is the offset of the item's first byte and end the offset just past it:
	// past the closing ] of a header, past the newline that ends the last physical
	// line of a variable or comment (or the end of the text).
	at, end int
	// name is a header's section name, empty for a header git would reject, or a
	// variable's name.
	name string
	// sub is whether a header has a quoted subsection.
	sub bool
	// open is whether a variable's value is still continued at the end of the text.
	open bool
}

// plainSection reports whether the item is a header of the section called name,
// without a subsection, compared case-insensitively.
func (it configItem) plainSection(name string) bool {
	return it.kind == configHeader && !it.sub && strings.EqualFold(strings.TrimSpace(it.name), name)
}

// endOfLine returns the offset just past the newline that ends the line the item
// ends on, where a header's line goes on with whatever follows it.
func (it configItem) endOfLine(config string) int {
	if it.end > 0 && config[it.end-1] == '\n' {
		return it.end
	}
	return configLineEnd(config, it.end)
}

// configItems walks git config text the way git reads it and yields its headers,
// variables and comments in order; blanks are skipped. A header and the variable
// after it on the same line are two items, as are headers that share a line. It
// never fails: a line git would reject becomes a comment, or a header without a
// name, and scanning goes on at the next line.
func configItems(config string) iter.Seq[configItem] {
	return func(yield func(configItem) bool) {
		i := 0
		if strings.HasPrefix(config, configBOM) {
			i = len(configBOM)
		}
		for i < len(config) {
			c := config[i]
			var it configItem
			switch {
			case isConfigSpace(c):
				i++
				continue
			case c == ';' || c == '#':
				it = configItem{kind: configComment, at: i, end: configLineEnd(config, i)}
			case c == '[':
				name, sub, end, ok := scanConfigHeader(config, i)
				if !ok {
					name, end = "", configLineEnd(config, i)
				}
				it = configItem{kind: configHeader, at: i, end: end, name: name, sub: sub}
			default:
				key, end, open := scanConfigVariable(config, i)
				it = configItem{kind: configVariable, at: i, end: end, name: key, open: open}
				if key == "" {
					it.kind = configComment
				}
			}
			if !yield(it) {
				return
			}
			i = it.end
		}
	}
}

// isConfigSpace reports the whitespace git skips in config text: space, tab, CR, LF.
func isConfigSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

// isConfigBlank reports whether s holds nothing but git whitespace.
func isConfigBlank(s string) bool {
	for i := range len(s) {
		if !isConfigSpace(s[i]) {
			return false
		}
	}
	return true
}

// configLineStart returns the offset of the start of the line holding offset i,
// after the byte order mark on the first line.
func configLineStart(config string, i int) int {
	if s := strings.LastIndexByte(config[:i], '\n') + 1; s != 0 {
		return s
	}
	if strings.HasPrefix(config, configBOM) {
		return len(configBOM)
	}
	return 0
}

// configLineEnd returns the offset just past the newline that ends the line
// holding offset i, or len(config) for a last line without one.
func configLineEnd(config string, i int) int {
	if n := strings.IndexByte(config[i:], '\n'); n >= 0 {
		return i + n + 1
	}
	return len(config)
}

// skipConfigBlanks returns the offset of the first byte at or after i that is not
// a space, tab or carriage return.
func skipConfigBlanks(config string, i int) int {
	for i < len(config) && config[i] != '\n' && isConfigSpace(config[i]) {
		i++
	}
	return i
}

// scanConfigHeader reads the section header that starts with the [ at offset i:
// its name, whether it has a quoted subsection (whose backslash escapes may hide a
// quote and whose text may hold a ]), and the offset just past its closing ]. It
// reports false for a header that does not close on its line.
func scanConfigHeader(config string, i int) (name string, sub bool, end int, ok bool) {
	j := i + 1
	for j < len(config) && config[j] != ']' && config[j] != '"' && config[j] != '\n' {
		j++
	}
	name = config[i+1 : j]
	if j < len(config) && config[j] == '"' {
		sub = true
		for j++; j < len(config) && config[j] != '"'; j++ {
			if config[j] == '\n' {
				return name, sub, 0, false
			}
			if config[j] == '\\' && j+1 < len(config) && config[j+1] != '\n' {
				j++
			}
		}
		j++
	}
	if j >= len(config) || config[j] != ']' {
		return name, sub, 0, false
	}
	return name, sub, j + 1, true
}

// scanConfigVariable reads the variable that starts at offset i and returns its
// name, empty when the line does not start with one, the offset just past its last
// physical line, and whether the text ends while its value is still continued.
func scanConfigVariable(config string, i int) (key string, end int, open bool) {
	j := i
	for j < len(config) && config[j] != '=' && !isConfigSpace(config[j]) {
		j++
	}
	if j == i {
		return "", configLineEnd(config, i), false
	}
	key = config[i:j]
	quoted := false
	for j < len(config) {
		open = false
		switch c := config[j]; {
		case c == '\n':
			return key, j + 1, false
		case c == '\\' && strings.HasPrefix(config[j+1:], "\r\n"):
			j += 3
			open = true
		case c == '\\' && j+1 < len(config):
			j += 2
			open = config[j-1] == '\n'
		case c == '\\':
			// git treats a backslash at the end of the text as a continuation.
			j++
			open = true
		case c == '"':
			quoted = !quoted
			j++
		case !quoted && (c == ';' || c == '#'):
			return key, configLineEnd(config, j), false
		default:
			j++
		}
	}
	return key, len(config), open
}

// configVariableValue returns the value that variable it, found in config, sets,
// decoded as git reads a value: quotes and escapes resolved, a continued
// value joined, a trailing comment dropped, unquoted whitespace dropped while the
// value is empty and before a comment or the line's end, and kept as written in
// between.
func configVariableValue(config string, it configItem) string {
	span := config[it.at:it.end]
	j := skipConfigBlanks(span, len(it.name))
	if j >= len(span) || span[j] != '=' {
		return ""
	}
	var b, pending []byte
	quoted, comment := false, false
	for j++; j < len(span); {
		c := span[j]
		j++
		if c == '\r' && j < len(span) && span[j] == '\n' {
			c = '\n'
			j++
		}
		switch {
		case c == '\n':
			return string(b)
		case comment:
			continue
		case isConfigSpace(c) && !quoted:
			if len(b) > 0 {
				pending = append(pending, c)
			}
			continue
		case !quoted && (c == ';' || c == '#'):
			comment = true
			continue
		}
		b, pending = append(b, pending...), pending[:0]
		switch c {
		case '\\':
			if j >= len(span) {
				return string(b)
			}
			c = span[j]
			j++
			if c == '\r' && j < len(span) && span[j] == '\n' {
				c = '\n'
				j++
			}
			switch c {
			case '\n':
				continue
			case 't':
				c = '\t'
			case 'b':
				c = '\b'
			case 'n':
				c = '\n'
			}
		case '"':
			quoted = !quoted
			continue
		}
		b = append(b, c)
	}
	return string(b)
}

// endConfigText returns config text ready for more text to be appended: it ends
// in a newline, and in an empty line when it ends inside a continued value, which
// would otherwise swallow what follows.
func endConfigText(text string) string {
	open := false
	for it := range configItems(text) {
		open = it.kind == configVariable && it.open
	}
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if open {
		text += "\n"
	}
	return text
}

// configEdit replaces config[from:to] with repl; insert marks text added after a
// run of items rather than a replacement of them.
type configEdit struct {
	from, to int
	repl     string
	insert   bool
}

// applyConfigEdits returns config with edits, which do not overlap, applied; edits
// at the same offset keep their order. An insertion first ends the text before it as endConfigText does.
func applyConfigEdits(config string, edits []configEdit) string {
	slices.SortStableFunc(edits, func(a, b configEdit) int { return a.from - b.from })
	var out strings.Builder
	copied := 0
	for _, e := range edits {
		out.WriteString(config[copied:e.from])
		copied = e.to
		if e.insert {
			s := out.String()
			if ended := endConfigText(s); ended != s {
				out.Reset()
				out.WriteString(ended)
			}
		}
		out.WriteString(e.repl)
	}
	out.WriteString(config[copied:])
	return out.String()
}

// dropConfigVariable returns the edit that removes variable it from config: its
// physical lines go. After headers on its own line it is the text after them that
// goes, and the headers stay, trimmed.
func dropConfigVariable(config string, it configItem) configEdit {
	from := configLineStart(config, it.at)
	if isConfigBlank(config[from:it.at]) {
		return configEdit{from: from, to: it.end}
	}
	return configEdit{from: from, to: it.end, repl: strings.TrimSpace(config[from:it.at]) + "\n"}
}
