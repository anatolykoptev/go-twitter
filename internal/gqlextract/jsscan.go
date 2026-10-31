package gqlextract

import "strings"

// Minimal scanners for the JS object/array literals that make up a compiled
// Relay artifact. They are NOT a JS parser: they only understand enough
// (bracket nesting, '"`-strings, escapes) to split a literal into top-level
// fields, which is what makes the x-web extraction independent of key order and
// of nested values (metadata:{}, providedVariables:{...}).

// matchClose returns the index of the bracket that closes the one opening at
// s[open] ('{' or '['), skipping string literals, or -1 when unbalanced.
func matchClose(s string, open int) int {
	if open < 0 || open >= len(s) || (s[open] != '{' && s[open] != '[') {
		return -1
	}
	depth := 0
	for i := open; i < len(s); i++ {
		switch c := s[i]; c {
		case '{', '[', '(':
			depth++
		case '}', ']', ')':
			depth--
			if depth == 0 {
				return i
			}
		case '"', '\'', '`':
			i = skipString(s, i)
			if i < 0 {
				return -1
			}
		}
	}
	return -1
}

// skipString returns the index of the quote closing the string opened at
// s[open], or -1. Template literals are treated as plain strings (Relay
// artifacts never interpolate).
func skipString(s string, open int) int {
	q := s[open]
	for i := open + 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case q:
			return i
		}
	}
	return -1
}

// field is one top-level key:value of an object literal; val is the raw source
// of the value.
type field struct{ key, val string }

// objectFields splits the object literal s (including its braces) into its
// top-level fields in source order. Computed or quoted keys are returned with
// their quotes stripped; malformed input yields whatever was parsed so far.
func objectFields(s string) []field {
	if len(s) < 2 || s[0] != '{' {
		return nil
	}
	body := s[1 : len(s)-1]
	var out []field
	for i := 0; i < len(body); {
		i = skipSpaceComma(body, i)
		if i >= len(body) {
			break
		}
		keyEnd := i
		for keyEnd < len(body) && body[keyEnd] != ':' && body[keyEnd] != ',' {
			if body[keyEnd] == '"' || body[keyEnd] == '\'' || body[keyEnd] == '`' {
				if keyEnd = skipString(body, keyEnd); keyEnd < 0 {
					return out
				}
			}
			keyEnd++
		}
		if keyEnd >= len(body) || body[keyEnd] != ':' { // shorthand {a,b}: no value
			i = keyEnd
			continue
		}
		key := strings.Trim(strings.TrimSpace(body[i:keyEnd]), "\"'`")
		valEnd := valueEnd(body, keyEnd+1)
		if valEnd < 0 {
			return out
		}
		out = append(out, field{key: key, val: strings.TrimSpace(body[keyEnd+1 : valEnd])})
		i = valEnd
	}
	return out
}

func skipSpaceComma(s string, i int) int {
	for i < len(s) && (s[i] == ',' || s[i] == ' ' || s[i] == '\n' || s[i] == '\t' || s[i] == '\r') {
		i++
	}
	return i
}

// valueEnd returns the index just past the value starting at from (the next
// top-level comma or end of s), or -1 when a nested literal is unbalanced.
func valueEnd(s string, from int) int {
	for i := from; i < len(s); i++ {
		switch s[i] {
		case ',':
			return i
		case '{', '[', '(':
			end := matchClose(s, i)
			if end < 0 {
				return -1
			}
			i = end
		case '"', '\'', '`':
			end := skipString(s, i)
			if end < 0 {
				return -1
			}
			i = end
		}
	}
	return len(s)
}

// fieldVal returns the raw value of key in fs.
func fieldVal(fs []field, key string) (string, bool) {
	for _, f := range fs {
		if f.key == key {
			return f.val, true
		}
	}
	return "", false
}

// stringLit unquotes a plain '"` string literal.
func stringLit(raw string) (string, bool) {
	if len(raw) < 2 {
		return "", false
	}
	switch q := raw[0]; q {
	case '"', '\'', '`':
		if raw[len(raw)-1] == q && !strings.ContainsRune(raw[1:len(raw)-1], rune(q)) {
			return raw[1 : len(raw)-1], true
		}
	}
	return "", false
}

// arrayElems splits an array literal (including brackets) into its raw
// top-level elements.
func arrayElems(s string) []string {
	if len(s) < 2 || s[0] != '[' {
		return nil
	}
	body := s[1 : len(s)-1]
	var out []string
	for i := 0; i < len(body); {
		i = skipSpaceComma(body, i)
		if i >= len(body) {
			break
		}
		end := valueEnd(body, i)
		if end < 0 {
			return out
		}
		out = append(out, strings.TrimSpace(body[i:end]))
		i = end
	}
	return out
}
