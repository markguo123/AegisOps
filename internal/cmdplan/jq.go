package cmdplan

import "strings"

// jq 表达式校验采用「明确支持的子集 + 白名单标识符」，不做不完整关键词黑名单：
// 词法层面只接受字面量、字段访问、数组/对象构造、括号、比较与算术运算符，
// 语法层面每个裸标识符必须是白名单里的纯函数；可疑输入能力（input/inputs、
// env/$ENV、debug/stderr、halt、import/include、字符串插值 \( ) 一律拒绝。
//
// 这不追求覆盖 jq 全集：无法校验的写法直接不放行（回落到人工审批或改用
// 结构化只读工具），宁可用不了也不用不安全。

// jqBools 是允许的布尔选项（不改变输入来源）。
var jqBools = map[byte]bool{
	'r': true, 'c': true, 'j': true, 'e': true, 'M': true,
	'C': true, 'a': true, 'S': true, 'n': true, 's': true, 'R': true,
}

var jqLongBools = map[string]bool{
	"--raw-output":        true,
	"--compact-output":    true,
	"--join-output":       true,
	"--exit-status":       true,
	"--monochrome-output": true,
	"--color-output":      true,
	"--ascii-output":      true,
	"--sort-keys":         true,
	"--null-input":        true,
	"--slurp":             true,
	"--raw-input":         true,
}

// jqFunctions 是允许出现的裸标识符（纯数据函数与关键字）。
var jqFunctions = map[string]bool{
	"length": true, "utf8bytelength": true, "keys": true, "keys_unsorted": true,
	"values": true, "type": true, "tostring": true, "tonumber": true,
	"tojson": true, "fromjson": true, "ascii_downcase": true, "ascii_upcase": true,
	"explode": true, "implode": true, "add": true, "any": true, "all": true,
	"min": true, "max": true, "min_by": true, "max_by": true, "sort": true,
	"sort_by": true, "group_by": true, "unique": true, "unique_by": true,
	"reverse": true, "flatten": true, "range": true, "floor": true, "ceil": true,
	"round": true, "sqrt": true, "fabs": true, "log": true, "exp": true, "pow": true,
	"isnan": true, "isnormal": true, "infinite": true, "nan": true,
	"select": true, "map": true, "map_values": true, "has": true, "in": true,
	"inside": true, "contains": true, "startswith": true, "endswith": true,
	"split": true, "join": true, "ltrimstr": true, "rtrimstr": true,
	"indices": true, "index": true, "rindex": true, "paths": true, "leaf_paths": true,
	"getpath": true, "setpath": true, "delpaths": true, "del": true,
	"to_entries": true, "from_entries": true, "with_entries": true, "path": true,
	"empty": true, "not": true, "first": true, "last": true, "nth": true,
	"limit": true, "until": true, "while": true, "isempty": true,
	"arrays": true, "objects": true, "iterables": true, "booleans": true,
	"numbers": true, "strings": true, "nulls": true, "scalars": true,
	"transpose": true, "walk": true, "combinations": true,
	"true": true, "false": true, "null": true,
	"and": true, "or": true, "as": true, "if": true, "then": true, "else": true,
	"elif": true, "end": true, "try": true, "catch": true, "reduce": true,
	"foreach": true,
}

// jqForbiddenIdents 是即便在白名单外也必须给出明确提示的输入/IO 能力。
var jqForbiddenIdents = map[string]bool{
	"input": true, "inputs": true, "debug": true, "stderr": true,
	"halt": true, "halt_error": true, "input_filename": true,
	"input_line_number": true, "modulemeta": true, "get_search_list": true,
	"builtins": true, "env": true, "recurse": true, "repeat": true,
}

func validateJq(args []string) error {
	var filter string
	positionals := 0
	for i := 0; i < len(args); {
		tok := args[i]
		switch {
		case tok == "--":
			return unsupportedf("jq 不接受 -- 之后的位置参数")
		case strings.HasPrefix(tok, "--"):
			name, _, hasValue := strings.Cut(tok, "=")
			if !jqLongBools[name] || hasValue {
				return unsupportedf("jq 参数 %q 不在只读自动执行集合内", name)
			}
			i++
		case len(tok) > 1 && tok[0] == '-':
			for j := 1; j < len(tok); j++ {
				if !jqBools[tok[j]] {
					return unsupportedf("jq 参数 -%c 不在只读自动执行集合内", tok[j])
				}
			}
			i++
		default:
			positionals++
			filter = tok
			i++
		}
	}
	if positionals == 0 {
		return unsupportedf("jq 必须显式给出过滤表达式")
	}
	if positionals > 1 {
		return unsupportedf("jq 不接受文件参数；只处理管道输入")
	}
	return validateJqExpression(filter)
}

// jqTokenKind 是 jq 表达式的词法类别。
type jqTokenKind int

const (
	jqIdent jqTokenKind = iota
	jqField             // . 之后的字段名
	jqNumber
	jqString
	jqVariable
	jqFormat
	jqPunct
	jqDot
	jqDotDot
)

type jqToken struct {
	kind jqTokenKind
	text string
	pos  int
}

func validateJqExpression(expr string) error {
	if strings.TrimSpace(expr) == "" {
		return unsupportedf("jq 过滤表达式为空")
	}
	tokens, err := lexJq(expr)
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		return unsupportedf("jq 过滤表达式为空")
	}
	if err := checkJqBalance(tokens); err != nil {
		return err
	}
	for i, tok := range tokens {
		switch tok.kind {
		case jqVariable:
			if tok.text == "__loc__" || tok.text == "ENV" {
				return unsupportedf("jq 表达式不允许使用 %s", "$"+tok.text)
			}
		case jqIdent:
			if jqForbiddenIdents[tok.text] {
				return unsupportedf("jq 表达式不允许使用 %s", tok.text)
			}
			prevIsField := i > 0 && tokens[i-1].kind == jqDot
			if prevIsField {
				continue
			}
			if i+1 < len(tokens) && tokens[i+1].kind == jqPunct && tokens[i+1].text == ":" {
				continue // 对象构造的键
			}
			if !jqFunctions[tok.text] {
				return unsupportedf("jq 表达式使用了不受支持的名字 %q", tok.text)
			}
		}
	}
	return nil
}

func checkJqBalance(tokens []jqToken) error {
	var stack []string
	for _, tok := range tokens {
		if tok.kind != jqPunct {
			continue
		}
		switch tok.text {
		case "(", "[", "{":
			stack = append(stack, tok.text)
		case ")", "]", "}":
			if len(stack) == 0 {
				return unsupportedf("jq 表达式括号不匹配")
			}
			open := stack[len(stack)-1]
			if (open == "(" && tok.text != ")") || (open == "[" && tok.text != "]") || (open == "{" && tok.text != "}") {
				return unsupportedf("jq 表达式括号不匹配")
			}
			stack = stack[:len(stack)-1]
		}
	}
	if len(stack) != 0 {
		return unsupportedf("jq 表达式括号不匹配")
	}
	return nil
}

// lexJq 把表达式切成白名单词法单元；任何不认识的字符都直接拒绝。
func lexJq(expr string) ([]jqToken, error) {
	var tokens []jqToken
	runes := []rune(expr)
	for i := 0; i < len(runes); {
		r := runes[i]
		switch {
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			i++
		case r == '.':
			if i+1 < len(runes) && runes[i+1] == '.' {
				tokens = append(tokens, jqToken{kind: jqDotDot, text: "..", pos: i})
				i += 2
				continue
			}
			tokens = append(tokens, jqToken{kind: jqDot, text: ".", pos: i})
			i++
		case isJqIdentStart(r):
			start := i
			for i < len(runes) && isJqIdentPart(runes[i]) {
				i++
			}
			text := string(runes[start:i])
			kind := jqIdent
			if len(tokens) > 0 && tokens[len(tokens)-1].kind == jqDot {
				kind = jqField
			}
			tokens = append(tokens, jqToken{kind: kind, text: text, pos: start})
		case r >= '0' && r <= '9':
			start := i
			for i < len(runes) && (runes[i] >= '0' && runes[i] <= '9' || runes[i] == '.' || runes[i] == 'e' || runes[i] == 'E') {
				i++
			}
			tokens = append(tokens, jqToken{kind: jqNumber, text: string(runes[start:i]), pos: start})
		case r == '"':
			start := i
			i++
			closed := false
			for i < len(runes) {
				if runes[i] == '\\' {
					if i+1 < len(runes) && runes[i+1] == '(' {
						return nil, unsupportedf("jq 表达式不允许字符串插值")
					}
					i += 2
					continue
				}
				if runes[i] == '"' {
					i++
					closed = true
					break
				}
				i++
			}
			if !closed {
				return nil, unsupportedf("jq 表达式字符串未闭合")
			}
			tokens = append(tokens, jqToken{kind: jqString, text: string(runes[start:i]), pos: start})
		case r == '$':
			start := i
			i++
			if i < len(runes) && isJqIdentStart(runes[i]) {
				for i < len(runes) && isJqIdentPart(runes[i]) {
					i++
				}
				tokens = append(tokens, jqToken{kind: jqVariable, text: string(runes[start+1 : i]), pos: start})
				continue
			}
			return nil, unsupportedf("jq 表达式变量引用不合法")
		case r == '@':
			start := i
			i++
			for i < len(runes) && isJqIdentPart(runes[i]) {
				i++
			}
			tokens = append(tokens, jqToken{kind: jqFormat, text: string(runes[start:i]), pos: start})
		case strings.ContainsRune("[]{}(),:|+-*/%<>=!?&", r):
			// 合并双字符运算符，避免把 <= 当成 < 与 = 两个无关符号。
			text := string(r)
			if i+1 < len(runes) && strings.ContainsRune("=/|", runes[i+1]) {
				text += string(runes[i+1])
				i++
			}
			tokens = append(tokens, jqToken{kind: jqPunct, text: text, pos: i})
			i++
		default:
			return nil, unsupportedf("jq 表达式包含不支持的字符 %q", string(r))
		}
	}
	return tokens, nil
}

func isJqIdentStart(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isJqIdentPart(r rune) bool {
	return isJqIdentStart(r) || (r >= '0' && r <= '9')
}
