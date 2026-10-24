// Package cli implements the App Stack command, not an API SDK.
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

type object = map[string]any
type graph struct {
	Version      int      `json:"version"`
	Models       []object `json:"models"`
	Tools        []object `json:"tools"`
	Agents       []object `json:"agents"`
	Applications []object `json:"applications"`
}

func emptyGraph() graph { return graph{1, []object{}, []object{}, []object{}, []object{}} }
func (g graph) group(kind string) []object {
	switch kind {
	case "model":
		return g.Models
	case "tool":
		return g.Tools
	case "application":
		return g.Applications
	default:
		return g.Agents
	}
}

type Diagnostic struct {
	Category string `json:"category"`
	File     string `json:"file"`
	Message  string `json:"message"`
}
type FileError struct{ Diagnostic Diagnostic }

func (e *FileError) Error() string { return e.Diagnostic.File + ": " + e.Diagnostic.Message }

type CommandError struct {
	Code, Message string
	Exit          int
}

func (e *CommandError) Error() string                   { return e.Message }
func commandError(code, message string, exit int) error { return &CommandError{code, message, exit} }

type fileErrors struct{ Errors []error }

func (e *fileErrors) Error() string {
	var ss []string
	for _, v := range e.Errors {
		ss = append(ss, v.Error())
	}
	return strings.Join(ss, "\n")
}
func fail(path, reason string, category ...string) {
	c := "schema"
	if len(category) > 0 {
		c = category[0]
	}
	panic(&FileError{Diagnostic{c, path, reason}})
}
func collect(errs *[]error, f func()) {
	if err := caught(f); err != nil {
		*errs = append(*errs, err)
	}
}
func caught(f func()) (err error) {
	defer func() {
		if e := recover(); e != nil {
			if v, ok := e.(*FileError); ok {
				err = v
			} else {
				panic(e)
			}
		}
	}()
	f()
	return
}

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
var contractRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,62}$`)
var envRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var jsonNumberRE = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

func idValid(value, prefix string) bool {
	ok, _ := regexp.MatchString(`^`+prefix+`_[0-9A-HJKMNP-TV-Z]{16}$`, value)
	return ok
}
func obj(v any) object { m, _ := v.(map[string]any); return m }
func str(v any) string { s, _ := v.(string); return s }
func list(v any) []any { a, _ := v.([]any); return a }
func objects(v any) []object {
	var r []object
	for _, x := range list(v) {
		r = append(r, obj(x))
	}
	return r
}
func fields(m object, names ...string) object {
	r := object{}
	for _, k := range names {
		if v, ok := m[k]; ok {
			r[k] = v
		}
	}
	return r
}
func wire(v any) string {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return "null"
	}
	s := strings.TrimSuffix(b.String(), "\n")
	return escapeC1(s)
}
func escapeC1(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0x7f && r <= 0x9f {
			fmt.Fprintf(&b, `\u%04x`, r)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
func literal(v any) string { s := wire(fmt.Sprint(v)); return s[1 : len(s)-1] }
func decodeJSON(raw []byte, strict bool) (any, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("invalid JSON")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var parse func() (any, error)
	parse = func() (any, error) {
		tok, err := d.Token()
		if err != nil {
			return nil, err
		}
		del, ok := tok.(json.Delim)
		if !ok {
			return tok, nil
		}
		switch del {
		case '{':
			m := object{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return nil, e
				}
				s, ok := k.(string)
				if !ok {
					return nil, errors.New("invalid JSON")
				}
				if _, ok := m[s]; ok && strict {
					return nil, errors.New("duplicate JSON field")
				}
				v, e := parse()
				if e != nil {
					return nil, e
				}
				m[s] = v
			}
			_, err = d.Token()
			return m, err
		case '[':
			a := []any{}
			for d.More() {
				v, e := parse()
				if e != nil {
					return nil, e
				}
				a = append(a, v)
			}
			_, err = d.Token()
			return a, err
		}
		return nil, errors.New("invalid JSON")
	}
	v, err := parse()
	if err != nil {
		return nil, errors.New("invalid JSON")
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, errors.New("invalid JSON")
	}
	return v, nil
}

// decimalParts compares exponents without allocating powers of ten from untrusted input.
func decimalParts(n json.Number) (digits string, power *big.Int, negative bool, ok bool) {
	s := string(n)
	if !jsonNumberRE.MatchString(s) {
		return "", nil, false, false
	}
	negative = strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == 'e' || r == 'E' })
	mant := parts[0]
	power = new(big.Int)
	if len(parts) > 1 {
		if _, ok = power.SetString(strings.TrimPrefix(parts[1], "+"), 10); !ok {
			return "", nil, false, false
		}
	}
	if i := strings.IndexByte(mant, '.'); i >= 0 {
		power.Sub(power, big.NewInt(int64(len(mant)-i-1)))
		mant = mant[:i] + mant[i+1:]
	}
	digits = strings.TrimLeft(mant, "0")
	if digits == "" {
		return "0", big.NewInt(0), negative, true
	}
	trim := strings.TrimRight(digits, "0")
	power.Add(power, big.NewInt(int64(len(digits)-len(trim))))
	return trim, power, negative, true
}
func exactInt(v any, max int64, minimum int64) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		switch x := v.(type) {
		case int:
			return int64(x), int64(x) >= minimum && int64(x) <= max
		case int64:
			return x, x >= minimum && x <= max
		}
		return 0, false
	}
	d, p, neg, ok := decimalParts(n)
	if !ok || p.Sign() < 0 {
		return 0, false
	}
	if d == "0" {
		return 0, minimum <= 0
	}
	if !p.IsInt64() || p.Int64() > 19 || int64(len(d))+p.Int64() > 19 {
		return 0, false
	}
	s := d + strings.Repeat("0", int(p.Int64()))
	if neg {
		s = "-" + s
	}
	i, err := strconv.ParseInt(s, 10, 64)
	return i, err == nil && i >= minimum && i <= max
}

const maxSafe int64 = 9007199254740991

func numberIs(v any, n int64) bool { i, ok := exactInt(v, maxSafe, -maxSafe); return ok && i == n }
func strictObject(v any, allowed, required []string, path string) object {
	m := obj(v)
	if m == nil {
		fail(path, "invalid or unknown fields")
	}
	for k, x := range m {
		if !slices.Contains(allowed, k) || x == nil {
			fail(path, "invalid or unknown fields")
		}
	}
	for _, k := range required {
		if _, ok := m[k]; !ok {
			fail(path, "invalid or unknown fields")
		}
	}
	return m
}
func text(v any, limit int, path string, required bool) {
	s, ok := v.(string)
	if !ok || len(s) > limit || strings.ContainsRune(s, 0) || (required && strings.TrimSpace(s) == "") {
		fail(path, "invalid text")
	}
}
func display(m object, path string) {
	text(m["display_name"], 1024, path, true)
	if utf8.RuneCountInString(str(m["display_name"])) > 256 {
		fail(path, "display_name too long")
	}
	if v, ok := m["description"]; ok {
		text(v, 16384, path, false)
	}
}
func collection(v any, path string, minimum int) []any {
	a, ok := v.([]any)
	if !ok || len(a) < minimum || len(a) > 100 {
		fail(path, "invalid collection")
	}
	return a
}
func boolean(v any, path string) {
	if _, ok := v.(bool); !ok {
		fail(path, "expected boolean")
	}
}
func positive(v any, path string, max int64) int64 {
	i, ok := exactInt(v, max, 1)
	if !ok {
		fail(path, "invalid positive integer")
	}
	return i
}
func technical(v any, path string, contract bool) {
	s, ok := v.(string)
	r := nameRE
	if contract {
		r = contractRE
	}
	if !ok || !r.MatchString(s) {
		fail(path, "invalid technical name")
	}
}
func envName(v any, path string) string {
	s, ok := v.(string)
	if !ok || !envRE.MatchString(s) {
		fail(path, "invalid environment locator")
	}
	return s
}
