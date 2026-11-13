package modules

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// msgDef is the catalog entry of a message code: its English template and
// whether only an administrator can receive it (the frontend keeps such
// texts in the administrator bundle).
type msgDef struct {
	en    string
	admin bool
}

// Msg is a text for display: a stable code and its parameters. Its English
// rendering (Text) is the text the CLI, the logs and the English fields of
// the API show; the browser translates it by its code ("srv." + code in the
// frontend's i18n files) and falls back to the English sent with it.
//
// A parameter value is a string, a number, a bool, a nested Msg or Problem
// (also as the JSON object a decoded message holds), or a list of these. A
// list is joined with "sep.problems" when its items are messages and with
// "sep.list" otherwise; a list in the parameter named "lines" is joined with
// "sep.lines" (one item per line, as errors.Join does).
//
// A Msg is an error whose text is the English rendering. It holds a map, so
// it must not be compared with == (as an error value that panics); compare
// codes instead.
type Msg struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
}

// M returns the message with the code and the parameters given as name,
// value pairs: M("rule.content.range", "min", 10, "max", 86400). Under "go
// test" it panics on an unknown code, an odd or malformed list of pairs, or
// parameters that differ from the placeholders of the template, so that a
// mistake fails a test instead of reaching a screen.
func M(code string, kv ...any) Msg {
	def, known := msgCatalog[code]
	if !known {
		strictFail("unknown message code %q", code)
	}
	if len(kv)%2 != 0 {
		strictFail("message %s: odd number of parameter arguments", code)
		kv = kv[:len(kv)-1]
	}
	var params map[string]any
	if len(kv) > 0 {
		params = make(map[string]any, len(kv)/2)
	}
	for i := 0; i < len(kv); i += 2 {
		name, ok := kv[i].(string)
		if !ok {
			strictFail("message %s: parameter name %v is not a string", code, kv[i])
			continue
		}
		params[name] = kv[i+1]
	}
	if known && testing.Testing() {
		want := placeholders(def.en)
		if len(want) != len(params) {
			strictFail("message %s: parameters %v, template has %v", code, keysOf(params), want)
		}
		for _, name := range want {
			if _, ok := params[name]; !ok {
				strictFail("message %s: parameter %s missing", code, name)
			}
		}
	}
	return Msg{Code: code, Params: params}
}

// strictFail panics while tests run and does nothing otherwise: in a
// running program a flawed message still renders as well as it can.
func strictFail(format string, args ...any) {
	if testing.Testing() {
		panic(fmt.Sprintf(format, args...))
	}
}

// Raw returns a message that is data only (a text of another program or
// library, an OS error): it is shown as it is in every language.
func Raw(text string) Msg {
	return Msg{Params: map[string]any{"text": text}}
}

// Text returns the English rendering of the message. An unknown code
// renders as the code itself.
func (m Msg) Text() string {
	if m.Code == "" {
		s, _ := m.Params["text"].(string)
		return s
	}
	def, ok := msgCatalog[m.Code]
	if !ok {
		return m.Code
	}
	return renderTemplate(def.en, m.Params)
}

// Error returns the English rendering, so that a Msg can be returned as an
// error.
func (m Msg) Error() string { return m.Text() }

// AsMsg returns the message of an error: the Msg or Problem itself, the
// lines of an errors.Join of such errors ("common.lines"), and otherwise
// the error's text as data (Raw). The English rendering of the result is
// always err.Error(); a wrapped Msg (fmt.Errorf with %w) is not unwrapped,
// because the wrapping text would be lost. A nil error gives an empty Raw.
func AsMsg(err error) Msg {
	if err == nil {
		return Raw("")
	}
	var m Msg
	switch e := err.(type) {
	case Msg:
		return e
	case *Msg:
		if e != nil {
			return *e
		}
	case Problem:
		m = M("common.problems", "problems", []Problem{e})
	case Problems:
		m = M("common.problems", "problems", e)
	case interface{ Unwrap() []error }:
		var lines []Msg
		for _, inner := range e.Unwrap() {
			if inner != nil {
				lines = append(lines, AsMsg(inner))
			}
		}
		m = M("common.lines", "lines", lines)
	}
	if m.Code != "" && m.Text() == err.Error() {
		return m
	}
	return Raw(err.Error())
}

// Problem is a message with its location: the parts of Where (a command, a
// pattern, a field ...) come before the message, each followed by ": ".
type Problem struct {
	Where []Msg `json:"where,omitempty"`
	Msg
}

// Text returns the English rendering: the location parts and the message
// joined with ": ".
func (p Problem) Text() string {
	parts := make([]string, 0, len(p.Where)+1)
	for _, w := range p.Where {
		parts = append(parts, w.Text())
	}
	return strings.Join(append(parts, p.Msg.Text()), ": ")
}

// Error returns the English rendering with the location.
func (p Problem) Error() string { return p.Text() }

// ProblemTexts returns the English rendering of each problem.
func ProblemTexts(ps []Problem) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Text()
	}
	return out
}

// Problems is a list of problems as one error: their English renderings
// joined with "; ".
type Problems []Problem

// Error returns the problems' English renderings joined with "; ".
func (ps Problems) Error() string {
	return strings.Join(ProblemTexts(ps), separator("sep.problems"))
}

// placeholders returns the names of the {{name}} placeholders of a template
// in order of first appearance.
func placeholders(tmpl string) []string {
	var names []string
	seen := map[string]bool{}
	for rest := tmpl; ; {
		i := strings.Index(rest, "{{")
		if i < 0 {
			return names
		}
		j := strings.Index(rest[i+2:], "}}")
		if j < 0 {
			return names
		}
		name := rest[i+2 : i+2+j]
		if validPlaceholder(name) && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
		rest = rest[i+2+j+2:]
	}
}

// validPlaceholder reports whether name is a placeholder name (letters,
// digits and underscores).
func validPlaceholder(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r == '_', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// renderTemplate replaces each {{name}} of the template with the English
// rendering of its parameter. A placeholder without a parameter is kept as
// it is.
func renderTemplate(tmpl string, params map[string]any) string {
	var b strings.Builder
	for rest := tmpl; ; {
		i := strings.Index(rest, "{{")
		if i < 0 {
			b.WriteString(rest)
			return b.String()
		}
		j := strings.Index(rest[i+2:], "}}")
		if j < 0 {
			b.WriteString(rest)
			return b.String()
		}
		name := rest[i+2 : i+2+j]
		b.WriteString(rest[:i])
		if v, ok := params[name]; ok && validPlaceholder(name) {
			b.WriteString(renderValue(name, v))
		} else {
			b.WriteString(rest[i : i+2+j+2])
		}
		rest = rest[i+2+j+2:]
	}
}

// renderValue returns the English rendering of a parameter value.
func renderValue(name string, v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case Msg:
		return x.Text()
	case *Msg:
		if x == nil {
			return ""
		}
		return x.Text()
	case Problem:
		return x.Text()
	case map[string]any:
		return msgFromMap(x).Text()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32)
	case fmt.Stringer:
		return x.String()
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return fmt.Sprint(v)
	}
	parts := make([]string, rv.Len())
	messages := false
	for i := range parts {
		item := rv.Index(i).Interface()
		messages = messages || isMessageValue(item)
		parts[i] = renderValue(name, item)
	}
	sep := "sep.list"
	switch {
	case name == "lines":
		sep = "sep.lines"
	case messages:
		sep = "sep.problems"
	}
	return strings.Join(parts, separator(sep))
}

// isMessageValue reports whether a list item is a message (as opposed to a
// value such as a name or a number).
func isMessageValue(v any) bool {
	switch v.(type) {
	case Msg, *Msg, Problem, map[string]any:
		return true
	}
	return false
}

// msgFromMap returns the problem a decoded JSON object holds ({"code",
// "params", "where"}), for parameters read back from stored JSON.
func msgFromMap(o map[string]any) Problem {
	var p Problem
	p.Code, _ = o["code"].(string)
	p.Params, _ = o["params"].(map[string]any)
	if where, ok := o["where"].([]any); ok {
		for _, w := range where {
			if wm, ok := w.(map[string]any); ok {
				p.Where = append(p.Where, msgFromMap(wm).Msg)
			}
		}
	}
	return p
}

// separator returns the English text of a separator code.
func separator(code string) string {
	return msgCatalog[code].en
}

// keysOf returns the keys of a parameter map (for panic texts).
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// IsMsg reports whether err is the message with the code (the Msg itself,
// not wrapped). It is the comparison to use instead of ==, which panics on a
// Msg.
func IsMsg(err error, code string) bool {
	m, ok := err.(Msg)
	return ok && m.Code == code
}
