package tools

import (
	"strconv"
	"strings"

	"github.com/k8s-ai/k8s-ai/internal/manifestx"
	"mvdan.cc/sh/v3/syntax"
)

// shellPolicy uses the same shell AST grammar as the execution environment.
// Every Kubernetes command must be independently authorized before approval.
func (s AccessScope) shellPolicy(command string) error {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil || len(file.Stmts) == 0 {
		return errUnproven
	}
	for _, stmt := range file.Stmts {
		if err := s.checkShellStatement(stmt, false); err != nil {
			return err
		}
	}
	return nil
}
func (s AccessScope) checkShellStatement(stmt *syntax.Stmt, filter bool) error {
	if stmt.Background || stmt.Coprocess || stmt.Negated || stmt.Disown {
		return errUnproven
	}
	switch cmd := stmt.Cmd.(type) {
	case *syntax.BinaryCmd:
		if len(stmt.Redirs) != 0 {
			return errUnproven
		}
		switch cmd.Op {
		case syntax.AndStmt, syntax.OrStmt:
			if filter {
				return errUnproven
			}
			if err := s.checkShellStatement(cmd.X, false); err != nil {
				return err
			}
			return s.checkShellStatement(cmd.Y, false)
		case syntax.Pipe:
			if err := s.checkShellStatement(cmd.X, filter); err != nil {
				return err
			}
			return s.checkShellStatement(cmd.Y, true)
		default:
			return errUnproven
		}
	case *syntax.CallExpr:
		if len(cmd.Assigns) != 0 || len(cmd.Args) == 0 {
			return errUnproven
		}
		args := make([]string, len(cmd.Args))
		for i, word := range cmd.Args {
			value, ok := literalParts(word.Parts)
			if !ok {
				return errUnproven
			}
			args[i] = value
		}
		if args[0] != "kubectl" {
			if !filter || len(stmt.Redirs) != 0 || len(args) != 3 || (args[0] != "head" && args[0] != "tail") || args[1] != "-n" {
				return errUnproven
			}
			n, err := strconv.Atoi(args[2])
			if err != nil || n < 1 || n > 1000 {
				return errUnproven
			}
			return nil
		}
		if len(stmt.Redirs) > 0 {
			if len(args) != 4 || args[2] != "-f" || args[3] != "-" || (args[1] != "apply" && args[1] != "create" && args[1] != "replace") {
				return errUnproven
			}
			if len(stmt.Redirs) != 1 || stmt.Redirs[0].Op != syntax.Hdoc || stmt.Redirs[0].Hdoc == nil {
				return errUnproven
			}
			manifest, ok := literalParts(stmt.Redirs[0].Hdoc.Parts)
			if !ok {
				return errUnproven
			}
			objs, err := manifestx.Parse(manifest, manifestx.Options{})
			if err != nil {
				return err
			}
			return s.checkObjects(objs)
		}
		return s.argvPolicy(args)
	default:
		return errUnproven
	}
}
func literalParts(parts []syntax.WordPart) (string, bool) {
	var result strings.Builder
	for _, part := range parts {
		switch value := part.(type) {
		case *syntax.Lit:
			// Wildcards can expand to files or extra Kubernetes targets at execution.
			if strings.ContainsAny(value.Value, "*?[\\") {
				return "", false
			}
			result.WriteString(value.Value)
		case *syntax.SglQuoted:
			if value.Dollar {
				return "", false
			}
			result.WriteString(value.Value)
		case *syntax.DblQuoted:
			if value.Dollar {
				return "", false
			}
			text, ok := literalParts(value.Parts)
			if !ok {
				return "", false
			}
			result.WriteString(text)
		default:
			return "", false
		}
	}
	return result.String(), true
}
