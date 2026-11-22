package config

// A deliberately small YAML subset parser so DoneGuard stays a dependency-free
// static binary. It supports what .doneguard.yml needs:
//
//   - block mappings and block sequences (including "- key: value" items)
//   - plain, 'single' and "double" quoted scalars
//   - flow sequences of scalars ([a, "b"]) and empty flow mappings ({})
//   - literal (|) and folded (>) block scalars with -/+ chomping
//   - comments (# preceded by whitespace, outside quotes)
//
// Anchors, multi-document streams and multi-line plain scalars are not
// supported and produce errors rather than silent misparses. Unlike strict
// YAML, plain scalars starting with * or ! (common in glob patterns such as
// **/id_rsa or !keep.env) are read as plain strings, not aliases or tags.

import (
	"fmt"
	"strconv"
	"strings"
)

type nodeKind int

const (
	nullNode nodeKind = iota
	scalarNode
	mapNode
	seqNode
)

type node struct {
	kind   nodeKind
	value  string
	quoted bool
	keys   []string
	vals   []*node
	items  []*node
	line   int
}

func (n *node) get(key string) *node {
	for i, k := range n.keys {
		if k == key {
			return n.vals[i]
		}
	}
	return nil
}

type srcLine struct {
	indent int
	text   string // comment-stripped, right-trimmed, without indentation
	raw    string
	num    int
	blank  bool
}

type parser struct {
	lines []srcLine
	pos   int
}

// YAMLError is a parse error with a 1-based line number.
type YAMLError struct {
	Line int
	Msg  string
}

func (e *YAMLError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("line %d: %s", e.Line, e.Msg)
	}
	return e.Msg
}

func errf(line int, format string, args ...any) error {
	return &YAMLError{Line: line, Msg: fmt.Sprintf(format, args...)}
}

func parseYAML(src string) (*node, error) {
	src = strings.TrimPrefix(src, "\ufeff")
	rawLines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	p := &parser{}
	for i, raw := range rawLines {
		raw = strings.TrimRight(raw, "\r")
		indent := 0
		for indent < len(raw) && raw[indent] == ' ' {
			indent++
		}
		rest := raw[indent:]
		if strings.HasPrefix(rest, "\t") {
			return nil, errf(i+1, "tab characters are not allowed for indentation")
		}
		text := strings.TrimRight(stripComment(rest), " \t")
		l := srcLine{indent: indent, text: text, raw: raw, num: i + 1, blank: text == ""}
		if !l.blank && indent == 0 && (text == "---" || text == "...") {
			if text == "..." || p.hasContent() {
				return nil, errf(i+1, "multiple YAML documents are not supported")
			}
			l.blank = true
		}
		p.lines = append(p.lines, l)
	}
	first, ok := p.peek()
	if !ok {
		return &node{kind: nullNode}, nil
	}
	n, err := p.parseNodeAt(first.indent)
	if err != nil {
		return nil, err
	}
	if l, ok := p.peek(); ok {
		return nil, errf(l.num, "unexpected content %q", l.text)
	}
	return n, nil
}

func (p *parser) hasContent() bool {
	for _, l := range p.lines {
		if !l.blank {
			return true
		}
	}
	return false
}

func (p *parser) peek() (srcLine, bool) {
	for p.pos < len(p.lines) && p.lines[p.pos].blank {
		p.pos++
	}
	if p.pos >= len(p.lines) {
		return srcLine{}, false
	}
	return p.lines[p.pos], true
}

func isSeqItem(text string) bool {
	return text == "-" || strings.HasPrefix(text, "- ")
}

func (p *parser) parseNodeAt(indent int) (*node, error) {
	l, ok := p.peek()
	if !ok {
		return &node{kind: nullNode}, nil
	}
	if isSeqItem(l.text) {
		return p.parseSeq(indent)
	}
	if _, _, isKey, err := splitKey(l.text, l.num); err != nil {
		return nil, err
	} else if isKey {
		return p.parseMap(indent)
	}
	p.pos++
	n, err := parseScalar(l.text, l.num)
	if err != nil {
		return nil, err
	}
	if next, ok := p.peek(); ok && next.indent > indent {
		return nil, errf(next.num, "multi-line plain scalars are not supported; quote the value or use a | block")
	}
	return n, nil
}

func (p *parser) parseMap(indent int) (*node, error) {
	m := &node{kind: mapNode}
	for {
		l, ok := p.peek()
		if !ok || l.indent < indent {
			break
		}
		if l.indent > indent {
			return nil, errf(l.num, "unexpected indentation")
		}
		if isSeqItem(l.text) {
			return nil, errf(l.num, "unexpected list item; expected \"key: value\"")
		}
		key, rest, isKey, err := splitKey(l.text, l.num)
		if err != nil {
			return nil, err
		}
		if !isKey {
			return nil, errf(l.num, "expected \"key: value\", got %q", l.text)
		}
		if m.get(key) != nil {
			return nil, errf(l.num, "duplicate key %q", key)
		}
		p.pos++
		var val *node
		switch {
		case rest == "":
			next, ok := p.peek()
			switch {
			case ok && next.indent > indent:
				val, err = p.parseNodeAt(next.indent)
			case ok && next.indent == indent && isSeqItem(next.text):
				val, err = p.parseSeq(indent)
			default:
				val = &node{kind: nullNode, line: l.num}
			}
		case rest[0] == '|' || rest[0] == '>':
			val, err = p.parseBlockScalar(rest, indent, l.num)
		default:
			val, err = parseScalar(rest, l.num)
		}
		if err != nil {
			return nil, err
		}
		if val.line == 0 {
			val.line = l.num
		}
		m.keys = append(m.keys, key)
		m.vals = append(m.vals, val)
	}
	return m, nil
}

func (p *parser) parseSeq(indent int) (*node, error) {
	s := &node{kind: seqNode}
	for {
		l, ok := p.peek()
		if !ok || l.indent < indent {
			break
		}
		if l.indent > indent {
			return nil, errf(l.num, "unexpected indentation")
		}
		if !isSeqItem(l.text) {
			break
		}
		rest := strings.TrimLeft(l.text[1:], " ")
		var item *node
		var err error
		if rest == "" {
			p.pos++
			next, ok := p.peek()
			if ok && next.indent > indent {
				item, err = p.parseNodeAt(next.indent)
			} else {
				item = &node{kind: nullNode, line: l.num}
			}
		} else {
			// Re-interpret the remainder of the line as if it started at its
			// own column; this handles "- key: value" maps and "- - nested".
			col := indent + (len(l.text) - len(rest))
			p.lines[p.pos].indent = col
			p.lines[p.pos].text = rest
			item, err = p.parseNodeAt(col)
		}
		if err != nil {
			return nil, err
		}
		if item.line == 0 {
			item.line = l.num
		}
		s.items = append(s.items, item)
	}
	return s, nil
}

func (p *parser) parseBlockScalar(header string, parentIndent, lineNum int) (*node, error) {
	style := header[0]
	chomp := byte(0)
	for _, c := range header[1:] {
		switch {
		case (c == '-' || c == '+') && chomp == 0:
			chomp = byte(c)
		case c >= '1' && c <= '9':
			return nil, errf(lineNum, "block scalar indentation indicators are not supported (%q); remove the digit", header)
		default:
			return nil, errf(lineNum, "invalid block scalar header %q", header)
		}
	}
	var body []string
	contentIndent := -1
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if strings.TrimSpace(l.raw) == "" {
			body = append(body, "")
			p.pos++
			continue
		}
		ind := len(l.raw) - len(strings.TrimLeft(l.raw, " "))
		if ind <= parentIndent {
			break
		}
		if contentIndent < 0 {
			contentIndent = ind
		}
		if ind < contentIndent {
			return nil, errf(l.num, "inconsistent indentation in block scalar")
		}
		body = append(body, l.raw[contentIndent:])
		p.pos++
	}
	// Trailing blank lines belong to chomping, not content.
	trailing := 0
	for len(body) > 0 && body[len(body)-1] == "" {
		body = body[:len(body)-1]
		trailing++
	}
	var text string
	if style == '|' {
		text = strings.Join(body, "\n")
	} else {
		var b strings.Builder
		for i, line := range body {
			if i > 0 {
				switch {
				case line == "":
					b.WriteString("\n")
				case body[i-1] == "":
				default:
					b.WriteString(" ")
				}
			}
			b.WriteString(line)
		}
		text = b.String()
	}
	switch chomp {
	case '-':
	case '+':
		if len(body) > 0 {
			text += "\n"
		}
		text += strings.Repeat("\n", trailing)
	default:
		if len(body) > 0 {
			text += "\n"
		}
	}
	return &node{kind: scalarNode, value: text, quoted: true, line: lineNum}, nil
}

// stripComment removes a trailing "# comment" that is outside quotes and
// preceded by whitespace (or at the start of the text).
func stripComment(s string) string {
	inSingle, inDouble := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inDouble:
			if c == '\\' {
				i++
			} else if c == '"' {
				inDouble = false
			}
		case inSingle:
			if c == '\'' {
				inSingle = false
			}
		case c == '"' && (i == 0 || isQuoteStart(s, i)):
			inDouble = true
		case c == '\'' && (i == 0 || isQuoteStart(s, i)):
			inSingle = true
		case c == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t'):
			return s[:i]
		}
	}
	return s
}

// isQuoteStart reports whether a quote at s[i] opens a quoted scalar (as
// opposed to an apostrophe inside a plain scalar such as "don't").
func isQuoteStart(s string, i int) bool {
	prev := strings.TrimRight(s[:i], " \t")
	if prev == "" {
		return true
	}
	last := prev[len(prev)-1]
	return last == ':' || last == '-' || last == '[' || last == ',' || last == '{'
}

// splitKey splits "key: rest" (key may be quoted). isKey is false when the
// text is not a mapping entry.
func splitKey(text string, line int) (key, rest string, isKey bool, err error) {
	if text == "" || text[0] == '[' || text[0] == '{' {
		return "", "", false, nil
	}
	if text[0] == '"' || text[0] == '\'' {
		end := closingQuote(text)
		if end < 0 {
			return "", "", false, nil
		}
		after := text[end+1:]
		if !strings.HasPrefix(after, ":") || (len(after) > 1 && after[1] != ' ' && after[1] != '\t') {
			return "", "", false, nil
		}
		k, err := parseScalar(text[:end+1], line)
		if err != nil {
			return "", "", false, err
		}
		return k.value, strings.TrimSpace(after[1:]), true, nil
	}
	for i := 0; i < len(text); i++ {
		if text[i] == ':' && (i == len(text)-1 || text[i+1] == ' ' || text[i+1] == '\t') {
			k := strings.TrimSpace(text[:i])
			if k == "" {
				return "", "", false, errf(line, "empty mapping key")
			}
			return k, strings.TrimSpace(text[i+1:]), true, nil
		}
	}
	return "", "", false, nil
}

func closingQuote(s string) int {
	q := s[0]
	for i := 1; i < len(s); i++ {
		if q == '"' && s[i] == '\\' {
			i++
			continue
		}
		if s[i] == q {
			if q == '\'' && i+1 < len(s) && s[i+1] == '\'' {
				i++
				continue
			}
			return i
		}
	}
	return -1
}

func parseScalar(s string, line int) (*node, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "" || s == "~" || s == "null" || s == "Null" || s == "NULL":
		return &node{kind: nullNode, line: line}, nil
	case s[0] == '"':
		end := closingQuote(s)
		if end != len(s)-1 {
			return nil, errf(line, "unterminated or malformed double-quoted string")
		}
		v, err := unescapeDouble(s[1:end])
		if err != nil {
			return nil, errf(line, "%v", err)
		}
		return &node{kind: scalarNode, value: v, quoted: true, line: line}, nil
	case s[0] == '\'':
		end := closingQuote(s)
		if end != len(s)-1 {
			return nil, errf(line, "unterminated or malformed single-quoted string")
		}
		return &node{kind: scalarNode, value: strings.ReplaceAll(s[1:end], "''", "'"), quoted: true, line: line}, nil
	case s[0] == '[':
		return parseFlowSeq(s, line)
	case s[0] == '{':
		if len(s) >= 2 && s[len(s)-1] == '}' && strings.TrimSpace(s[1:len(s)-1]) == "" {
			return &node{kind: mapNode, line: line}, nil
		}
		return nil, errf(line, "flow mappings are not supported; use block style")
	case s[0] == '&':
		return nil, errf(line, "anchors and aliases are not supported")
	case s[0] == '!':
		return nil, errf(line, "tags are not supported")
	}
	return &node{kind: scalarNode, value: s, line: line}, nil
}

func parseFlowSeq(s string, line int) (*node, error) {
	if s[len(s)-1] != ']' {
		return nil, errf(line, "unterminated flow sequence")
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	seq := &node{kind: seqNode, line: line}
	if inner == "" {
		return seq, nil
	}
	var parts []string
	start := 0
	for i := 0; i < len(inner); i++ {
		switch inner[i] {
		case '"', '\'':
			end := closingQuote(inner[i:])
			if end < 0 {
				return nil, errf(line, "unterminated string in flow sequence")
			}
			i += end
		case '[', '{':
			return nil, errf(line, "nested flow collections are not supported")
		case ',':
			parts = append(parts, inner[start:i])
			start = i + 1
		}
	}
	parts = append(parts, inner[start:])
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			continue // allow trailing comma
		}
		item, err := parseScalar(part, line)
		if err != nil {
			return nil, err
		}
		seq.items = append(seq.items, item)
	}
	return seq, nil
}

func unescapeDouble(s string) (string, error) {
	if !strings.Contains(s, `\`) {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(s) {
			return "", fmt.Errorf("trailing backslash in string")
		}
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '0':
			b.WriteByte(0)
		case '\\', '"', '/', ' ':
			b.WriteByte(s[i])
		case 'u':
			if i+4 >= len(s) {
				return "", fmt.Errorf("short \\u escape")
			}
			r, err := strconv.ParseUint(s[i+1:i+5], 16, 32)
			if err != nil {
				return "", fmt.Errorf("bad \\u escape")
			}
			b.WriteRune(rune(r))
			i += 4
		default:
			return "", fmt.Errorf("unsupported escape \\%c", s[i])
		}
	}
	return b.String(), nil
}
