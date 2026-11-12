// Package cmdplan 把一条 kubectl 命令文本分析成一份可复用的结论
// （方案 24《只读命令组合与 Secret 诊断权限优化》§3.1）：
//
//   - 是否需要 shell 解释器；
//   - 是否可证明只读；
//   - 每条 Kubernetes 调用的 argv 与目标（namespace/资源/动作）；
//   - 可证明只读时，可直接交给执行器的受控 argv 程序。
//
// 审批、平台策略、旧范围校验与执行器必须复用同一份结论，不能各自用字符串
// 切分生成互相矛盾的判断。本包不 import approval/tools/executor，也不做风险
// 分级（风险表留在 approval），因此不会制造循环依赖。
//
// 只读判定基于完整的 shell AST：顶层只允许单命令、管道 |、顺序 ;/换行、
// 条件串联 &&/||；每条分支必须以明确的 kubectl 只读命令开始，后续只能是参数
// 受控的过滤器。动态命令名、变量展开、命令/进程替换、通配符、重定向、后台
// 任务、循环、函数、环境赋值与 shell 包装器一律不进入自动执行集合。
package cmdplan

import (
	"errors"
	"fmt"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Op 是两条命令之间的连接语义（沿用 shell 的短路规则）。
type Op int

const (
	// OpSeq 是无条件顺序执行（; 或换行）。
	OpSeq Op = iota
	// OpAnd 是前一条成功才执行（&&）。
	OpAnd
	// OpOr 是前一条失败才执行（||）。
	OpOr
)

// String 返回 shell 写法，便于文案与测试断言。
func (o Op) String() string {
	switch o {
	case OpAnd:
		return "&&"
	case OpOr:
		return "||"
	default:
		return ";"
	}
}

// StageKind 区分 Kubernetes 调用与本地过滤器。
type StageKind int

const (
	// StageKubectl 是一段 kubectl 只读调用。
	StageKubectl StageKind = iota
	// StageFilter 是只处理 stdin 的过滤器（grep/head/jq 等）。
	StageFilter
)

// Stage 是管道中的一段：Argv[0] 对 kubectl 段恒为 "kubectl"。
type Stage struct {
	Kind StageKind
	Argv []string
}

// Command 是一条管线及其连接语义。
type Command struct {
	Op     Op
	Stages []Stage
}

// Program 是一条命令的可受控执行计划：不含任何 shell 结构，只有
// 「若干条 argv 管线 + 顺序/条件语义」，由 executor 直接逐段 fork。
type Program struct {
	Commands []Command
}

// 调用动作（与 authz 的 ActionRead/ActionLogs 对应，避免本包 import authz）。
const (
	ActionRead = "read"
	ActionLogs = "logs"
)

// Call 是一条 Kubernetes 调用的结构化目标。
type Call struct {
	// Argv 是完整 argv（含 argv[0]="kubectl"）。
	Argv []string
	// Verb 是 kubectl 动词；rollout 的只读子命令写进 Subcommand。
	Verb       string
	Subcommand string
	// Kind 是命令里书写的资源（可能是 pods、deploy/name、pods,svc，未解析别名）。
	Kind string
	// Name 是资源名（未指定时为空）。
	Name string
	// Namespace 是 -n/--namespace 的取值；未被显式指定时为空。
	Namespace string
	// AllNamespaces 表示使用 -A/--all-namespaces。
	AllNamespaces bool
	// Action 是本调用需要的平台权限动作。
	Action string
	// CallLine 是该调用所在的完整 kubectl 命令文本（审批卡片定位用）。
	CallLine string
}

// Analysis 是一次命令分析的完整结论。
type Analysis struct {
	// NeedsShell 表示命令含管道/串联等 shell 结构，按原样执行需要解释器。
	// 可证明只读时执行器走受控 argv 程序，不使用该解释器。
	NeedsShell bool
	// ProvablyReadOnly 表示整条命令已被证明是只读的，可自动执行。
	ProvablyReadOnly bool
	// Calls 是本次分析识别到的全部 Kubernetes 调用（可能不完整）。
	Calls []Call
	// Program 只在 ProvablyReadOnly 为 true 时有效。
	Program Program
	// Reason 说明为什么不能证明只读（可证明只读时为空），用于审批与拒绝文案。
	Reason string
}

var (
	errUnsupported = errors.New("unsupported shell construct")
	errRedirect    = errors.New("redirection is not allowed in read-only combinations")
	errExpansion   = errors.New("dynamic expansion (variables, substitutions or wildcards) is not allowed")
	errAssignment  = errors.New("environment assignment is not allowed")
	errPipeline    = errors.New("pipeline must start with a kubectl read-only call")
)

// Analyze 分析一条命令。任何解析失败或未支持的构造都只会让结果
// ProvablyReadOnly=false，不会 panic，也不会误判为可自动执行。
func Analyze(command string) Analysis {
	command = strings.TrimSpace(command)
	if command == "" {
		return Analysis{Reason: "命令为空"}
	}
	if len(command) > 4096 {
		return Analysis{Reason: "命令超过 4096 字节上限"}
	}
	parser := syntax.NewParser(syntax.Variant(syntax.LangBash), syntax.KeepComments(false))
	file, err := parser.Parse(strings.NewReader(command), "")
	if err != nil {
		return Analysis{NeedsShell: true, Reason: "无法按 shell 语法完整解析命令"}
	}
	if len(file.Stmts) == 0 {
		return Analysis{Reason: "命令不含可执行语句"}
	}
	b := &builder{}
	for _, stmt := range file.Stmts {
		if err := b.cmd(stmt); err != nil {
			a := Analysis{Calls: b.calls, NeedsShell: true, Reason: reasonText(err)}
			return a
		}
	}
	a := Analysis{Calls: b.calls}
	if b.programEmpty() {
		return Analysis{Calls: b.calls, Reason: "命令不含可执行语句"}
	}
	a.Program = b.program
	a.ProvablyReadOnly = true
	a.NeedsShell = len(b.program.Commands) > 1 || len(b.program.Commands[0].Stages) > 1
	return a
}

// reasonText 把内部错误翻译成稳定的中文原因（写进审批与拒绝文案）。
func reasonText(err error) string {
	var unknown unknownKindError
	if errors.As(err, &unknown) {
		return unknown.reason
	}
	switch err {
	case errUnsupported:
		return "包含 shell 循环/分组/函数等未支持构造"
	case errRedirect:
		return "包含重定向"
	case errExpansion:
		return "包含变量展开、命令替换或通配符"
	case errAssignment:
		return "包含环境变量赋值"
	case errPipeline:
		return "每条查询必须以 kubectl 只读命令开始"
	default:
		return err.Error()
	}
}

// builder 逐语句构建 Program 并收集调用目标。
type builder struct {
	program Program
	pending Op
	calls   []Call
}

func (b *builder) programEmpty() bool { return len(b.program.Commands) == 0 }

func (b *builder) push(stages []Stage) error {
	if len(stages) == 0 || stages[0].Kind != StageKubectl {
		return errPipeline
	}
	b.program.Commands = append(b.program.Commands, Command{Op: b.pending, Stages: stages})
	b.pending = OpSeq
	return nil
}

// cmd 递归展开一条语句（顶层顺序/条件/管道）。
func (b *builder) cmd(stmt *syntax.Stmt) error {
	if stmt == nil {
		return errUnsupported
	}
	if stmt.Background || stmt.Coprocess || stmt.Negated || stmt.Disown {
		return errUnsupported
	}
	switch c := stmt.Cmd.(type) {
	case *syntax.BinaryCmd:
		if len(stmt.Redirs) != 0 {
			return errRedirect
		}
		switch c.Op {
		case syntax.AndStmt, syntax.OrStmt:
			if err := b.cmd(c.X); err != nil {
				return err
			}
			if c.Op == syntax.OrStmt {
				b.pending = OpOr
			} else {
				b.pending = OpAnd
			}
			return b.cmd(c.Y)
		case syntax.Pipe:
			stages, err := b.pipeline(stmt)
			if err != nil {
				return err
			}
			return b.push(stages)
		default:
			return errUnsupported
		}
	case *syntax.CallExpr:
		st, err := b.stage(stmt)
		if err != nil {
			return err
		}
		return b.push([]Stage{st})
	default:
		return errUnsupported
	}
}

// pipeline 展开一条管道链（左结合），每段都必须是单条调用。
func (b *builder) pipeline(stmt *syntax.Stmt) ([]Stage, error) {
	c, ok := stmt.Cmd.(*syntax.BinaryCmd)
	if !ok || c.Op != syntax.Pipe {
		return nil, errUnsupported
	}
	if len(stmt.Redirs) != 0 {
		return nil, errRedirect
	}
	var stages []Stage
	if left, ok := c.X.Cmd.(*syntax.BinaryCmd); ok && left.Op == syntax.Pipe {
		inner, err := b.pipeline(c.X)
		if err != nil {
			return nil, err
		}
		stages = append(stages, inner...)
	} else {
		st, err := b.stage(c.X)
		if err != nil {
			return nil, err
		}
		stages = append(stages, st)
	}
	st, err := b.stage(c.Y)
	if err != nil {
		return nil, err
	}
	return append(stages, st), nil
}

// stage 校验一段调用并返回 Stage。
func (b *builder) stage(stmt *syntax.Stmt) (Stage, error) {
	if stmt == nil {
		return Stage{}, errUnsupported
	}
	if len(stmt.Redirs) != 0 {
		return Stage{}, errRedirect
	}
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok {
		return Stage{}, errUnsupported
	}
	if len(call.Assigns) != 0 {
		return Stage{}, errAssignment
	}
	if len(call.Args) == 0 {
		return Stage{}, errUnsupported
	}
	argv := make([]string, len(call.Args))
	for i, word := range call.Args {
		value, ok := literalWord(word, false)
		if !ok {
			return Stage{}, errExpansion
		}
		argv[i] = value
	}
	if argv[0] == "kubectl" {
		target, err := kubectlCall(argv)
		if err != nil {
			return Stage{}, err
		}
		b.calls = append(b.calls, target)
		return Stage{Kind: StageKubectl, Argv: argv}, nil
	}
	if err := validateFilter(argv); err != nil {
		return Stage{}, err
	}
	return Stage{Kind: StageFilter, Argv: argv}, nil
}

// literalWord 把 shell Word 折叠成字面量字符串。任何动态展开都返回 false：
// 变量、命令替换、进程替换、算术展开、通配符与花括号展开都会让实际执行内容
// 偏离校验内容，因此一律不进入自动执行集合。
func literalWord(word *syntax.Word, quoted bool) (string, bool) {
	if word == nil {
		return "", false
	}
	var b strings.Builder
	for _, part := range word.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			if !quoted && strings.ContainsAny(p.Value, "*?[{") {
				return "", false
			}
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			if p.Dollar {
				return "", false
			}
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			if p.Dollar {
				return "", false
			}
			s, ok := literalParts(p.Parts)
			if !ok {
				return "", false
			}
			b.WriteString(s)
		default:
			return "", false
		}
	}
	return b.String(), true
}

// literalParts 折叠引号内的片段：引号里的 * ? [ 不参与通配符展开，可以保留。
func literalParts(parts []syntax.WordPart) (string, bool) {
	var b strings.Builder
	for _, part := range parts {
		word := &syntax.Word{Parts: []syntax.WordPart{part}}
		s, ok := literalWord(word, true)
		if !ok {
			return "", false
		}
		b.WriteString(s)
	}
	return b.String(), true
}

// errUnknownVerb 表示命令不是已知的 kubectl 只读动词（可能写、可能未知）。
type unknownKindError struct{ reason string }

func (e unknownKindError) Error() string { return e.reason }

func unsupportedf(format string, args ...any) error {
	return unknownKindError{reason: fmt.Sprintf(format, args...)}
}
