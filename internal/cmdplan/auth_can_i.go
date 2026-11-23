package cmdplan

import (
	"mvdan.cc/sh/v3/syntax"
	"strings"
)

// analyzeAuthCanI deliberately supports a single, explicitly scoped query for
// the current identity. Impersonation, --list and dynamic syntax are excluded.
func analyzeAuthCanI(command string) (Analysis, bool) {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil || len(f.Stmts) != 1 {
		return Analysis{}, false
	}
	st := f.Stmts[0]
	c, ok := st.Cmd.(*syntax.CallExpr)
	if !ok || st.Background || st.Negated || len(st.Redirs) > 0 || len(c.Assigns) > 0 {
		return Analysis{}, false
	}
	var a []string
	for _, w := range c.Args {
		v, ok := literalWord(w, false)
		if !ok {
			return Analysis{}, false
		}
		a = append(a, v)
	}
	if len(a) < 5 || a[0] != "kubectl" || a[1] != "auth" || a[2] != "can-i" {
		return Analysis{}, false
	}
	verbs := map[string]bool{"get": true, "list": true, "watch": true, "create": true, "update": true, "patch": true, "delete": true}
	if !verbs[a[3]] || !execIdentifier.MatchString(a[4]) {
		return Analysis{}, false
	}
	call := Call{Argv: a, Verb: "auth", Subcommand: "can-i", Kind: a[4], Action: ActionRead, CallLine: command}
	for i := 5; i < len(a); i++ {
		k, v, eq := strings.Cut(a[i], "=")
		if k != "-n" && k != "--namespace" || call.Namespace != "" {
			return Analysis{}, false
		}
		if !eq {
			i++
			if i >= len(a) {
				return Analysis{}, false
			}
			v = a[i]
		}
		if !execIdentifier.MatchString(v) {
			return Analysis{}, false
		}
		call.Namespace = v
	}
	if call.Namespace == "" {
		return Analysis{}, false
	}
	return Analysis{ProvablyReadOnly: true, Calls: []Call{call}, Program: Program{Commands: []Command{{Stages: []Stage{{Kind: StageKubectl, Argv: a}}}}}}, true
}
