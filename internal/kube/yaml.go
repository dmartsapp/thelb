package kube

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// decodeYAML reads the YAML a kubeconfig is written in - block mappings and
// sequences, plain and quoted scalars, comments, and the empty flow
// collections {} and [] - into maps, slices and strings. JSON, which is YAML
// too, is read as JSON. Anything else - block scalars, anchors, multi-line
// plain scalars - is refused with its line: theLB keeps its dependencies to
// two, and a kubeconfig needs no more than this.
func decodeYAML(b []byte) (any, error) {
	text := strings.TrimSpace(string(b))
	if strings.HasPrefix(text, "{") {
		var v any
		if err := json.Unmarshal([]byte(text), &v); err != nil {
			return nil, fmt.Errorf("reading it as JSON: %w", err)
		}
		return v, nil
	}
	var ls []yline
	for i, raw := range strings.Split(string(b), "\n") {
		raw = strings.TrimRight(raw, " \t\r")
		if strings.Contains(raw, "\t") && strings.TrimLeft(raw, "\t") != raw {
			return nil, fmt.Errorf("line %d: indented with a tab", i+1)
		}
		content := strings.TrimLeft(raw, " ")
		if content == "" || strings.HasPrefix(content, "#") || content == "---" || content == "..." {
			continue
		}
		content = stripComment(content)
		ls = append(ls, yline{n: i + 1, indent: len(raw) - len(strings.TrimLeft(raw, " ")), text: content})
	}
	if len(ls) == 0 {
		return map[string]any{}, nil
	}
	p := &yparser{ls: ls}
	v, err := p.block(ls[0].indent)
	if err != nil {
		return nil, err
	}
	if p.i < len(p.ls) {
		return nil, fmt.Errorf("line %d: unexpected indentation", p.ls[p.i].n)
	}
	return v, nil
}

type yline struct {
	n      int
	indent int
	text   string
}

type yparser struct {
	ls []yline
	i  int
}

func isItem(text string) bool { return text == "-" || strings.HasPrefix(text, "- ") }

// block reads the mapping or sequence whose lines are at indent.
func (p *yparser) block(indent int) (any, error) {
	if isItem(p.ls[p.i].text) {
		return p.sequence(indent)
	}
	return p.mapping(indent)
}

func (p *yparser) mapping(indent int) (any, error) {
	m := map[string]any{}
	for p.i < len(p.ls) {
		l := p.ls[p.i]
		if l.indent < indent || (l.indent == indent && isItem(l.text)) {
			break
		}
		if l.indent > indent {
			return nil, fmt.Errorf("line %d: unexpected indentation", l.n)
		}
		key, rest, ok := splitKey(l.text)
		if !ok {
			return nil, fmt.Errorf("line %d: expected \"key: value\"", l.n)
		}
		if _, dup := m[key]; dup {
			return nil, fmt.Errorf("line %d: %q twice", l.n, key)
		}
		p.i++
		if rest != "" {
			v, err := scalar(rest, l.n)
			if err != nil {
				return nil, err
			}
			m[key] = v
			continue
		}
		// A nested block: deeper, or a sequence at the key's own indentation, as
		// kubectl writes them.
		switch {
		case p.i < len(p.ls) && p.ls[p.i].indent > indent:
			v, err := p.block(p.ls[p.i].indent)
			if err != nil {
				return nil, err
			}
			m[key] = v
		case p.i < len(p.ls) && p.ls[p.i].indent == indent && isItem(p.ls[p.i].text):
			v, err := p.sequence(indent)
			if err != nil {
				return nil, err
			}
			m[key] = v
		default:
			m[key] = nil
		}
	}
	return m, nil
}

func (p *yparser) sequence(indent int) (any, error) {
	out := []any{}
	for p.i < len(p.ls) {
		l := p.ls[p.i]
		if l.indent != indent || !isItem(l.text) {
			if l.indent > indent {
				return nil, fmt.Errorf("line %d: unexpected indentation", l.n)
			}
			break
		}
		rest := strings.TrimLeft(strings.TrimPrefix(l.text, "-"), " ")
		if rest == "" {
			p.i++
			if p.i < len(p.ls) && p.ls[p.i].indent > indent {
				v, err := p.block(p.ls[p.i].indent)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			} else {
				out = append(out, nil)
			}
			continue
		}
		if _, _, ok := splitKey(rest); ok || isItem(rest) {
			// "- key: value": a mapping (or a sequence) whose first line shares
			// the dash's; its lines are where that first key is.
			p.ls[p.i] = yline{n: l.n, indent: indent + len(l.text) - len(rest), text: rest}
			v, err := p.block(p.ls[p.i].indent)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
			continue
		}
		v, err := scalar(rest, l.n)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		p.i++
	}
	return out, nil
}

// splitKey splits "key: value" or "key:"; a quoted key may hold ": ".
func splitKey(text string) (key, rest string, ok bool) {
	if strings.HasPrefix(text, `"`) || strings.HasPrefix(text, "'") {
		end := closingQuote(text)
		if end < 0 {
			return "", "", false
		}
		after := text[end+1:]
		if after != ":" && !strings.HasPrefix(after, ": ") {
			return "", "", false
		}
		k, err := scalar(text[:end+1], 0)
		if err != nil {
			return "", "", false
		}
		s, _ := k.(string)
		return s, strings.TrimSpace(strings.TrimPrefix(after, ":")), true
	}
	if strings.HasSuffix(text, ":") && !strings.Contains(text[:len(text)-1], ": ") {
		return text[:len(text)-1], "", true
	}
	i := strings.Index(text, ": ")
	if i <= 0 {
		return "", "", false
	}
	return text[:i], strings.TrimSpace(text[i+2:]), true
}

// closingQuote is the index of the quote closing the one text starts with, or
// -1.
func closingQuote(text string) int {
	q := text[0]
	for i := 1; i < len(text); i++ {
		switch {
		case q == '"' && text[i] == '\\':
			i++
		case text[i] == q && q == '\'' && i+1 < len(text) && text[i+1] == '\'':
			i++
		case text[i] == q:
			return i
		}
	}
	return -1
}

// scalar is a value on a line: quoted, an empty flow collection, or plain.
func scalar(text string, line int) (any, error) {
	switch {
	case text == "{}":
		return map[string]any{}, nil
	case text == "[]":
		return []any{}, nil
	case strings.HasPrefix(text, "{") || strings.HasPrefix(text, "["):
		var v any
		if err := json.Unmarshal([]byte(text), &v); err != nil {
			return nil, fmt.Errorf("line %d: a flow collection theLB does not read: write it as a block", line)
		}
		return v, nil
	case strings.HasPrefix(text, `"`):
		if closingQuote(text) != len(text)-1 {
			return nil, fmt.Errorf("line %d: a quoted value that does not end on its line", line)
		}
		s, err := strconv.Unquote(text)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		return s, nil
	case strings.HasPrefix(text, "'"):
		if closingQuote(text) != len(text)-1 {
			return nil, fmt.Errorf("line %d: a quoted value that does not end on its line", line)
		}
		return strings.ReplaceAll(text[1:len(text)-1], "''", "'"), nil
	case text == "|" || text == ">" || strings.HasPrefix(text, "|") || strings.HasPrefix(text, ">") ||
		strings.HasPrefix(text, "&") || strings.HasPrefix(text, "*") || strings.HasPrefix(text, "!"):
		return nil, fmt.Errorf("line %d: YAML theLB does not read (block scalars, anchors, tags): write the kubeconfig as kubectl does, or as JSON", line)
	}
	return text, nil
}

// stripComment removes a comment that follows a value: " #" outside quotes.
func stripComment(text string) string {
	var q byte
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case q != 0:
			if c == '\\' && q == '"' {
				i++
			} else if c == q {
				q = 0
			}
		case c == '"' || c == '\'':
			if i == 0 || text[i-1] == ' ' || text[i-1] == ':' || text[i-1] == '-' {
				q = c
			}
		case c == '#' && i > 0 && (text[i-1] == ' ' || text[i-1] == '\t'):
			return strings.TrimRight(text[:i], " \t")
		}
	}
	return text
}
