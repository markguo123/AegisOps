package cmdplan

import (
	"regexp"
	"strconv"
	"strings"
)

// 首批自动放行的过滤器：只处理管道 stdin，不能读取额外文件、写文件或启动子命令。
// 不认识的选项、缩写和参数形式一律不放行（回落到人工审批）。
//
// 首批不放行：awk、sed、xargs、find -exec、解释器、任意本地文件读取、
// curl/wget 等外连命令。

func validateFilter(argv []string) error {
	switch argv[0] {
	case "grep":
		return validateGrep(argv[1:])
	case "head", "tail":
		return validateHeadTail(argv[0], argv[1:])
	case "sort":
		return validateSort(argv[1:])
	case "uniq":
		return validateUniq(argv[1:])
	case "wc":
		return validateWordCount(argv[1:])
	case "cut":
		return validateCut(argv[1:])
	case "tr":
		return validateTr(argv[1:])
	case "jq":
		return validateJq(argv[1:])
	default:
		return unsupportedf("命令 %q 不在只读自动执行集合内", argv[0])
	}
}

// shortSpec 描述一组短选项：无值选项、取值选项以及取值回调。
type shortSpec struct {
	bools   map[byte]bool
	valued  map[byte]bool
	onValue func(c byte, value string) error
}

// parse 解析一个短选项 token（允许 -irv 这种合并写法），返回下一个下标。
func (s shortSpec) parse(args []string, i int) (int, error) {
	tok := args[i]
	for j := 1; j < len(tok); j++ {
		c := tok[j]
		if s.bools[c] {
			continue
		}
		if !s.valued[c] {
			return i, unsupportedf("参数 -%c 不在只读自动执行集合内", c)
		}
		value := tok[j+1:]
		if value == "" {
			i++
			if i >= len(args) {
				return i, unsupportedf("参数 -%c 缺少取值", c)
			}
			value = args[i]
		}
		if s.onValue != nil {
			if err := s.onValue(c, value); err != nil {
				return i, err
			}
		}
		return i + 1, nil
	}
	return i + 1, nil
}

// grep 只允许模式 + 影响匹配方式的布尔选项；任何第二个位置参数都是文件参数，
// 必须拒绝，不能用"模式/文件分不清"的方式放行。
var grepBools = map[byte]bool{
	'i': true, 'E': true, 'F': true, 'v': true, 'c': true, 'l': true,
	'n': true, 'H': true, 'h': true, 'o': true, 'w': true, 'x': true,
	'q': true, 's': true, 'a': true, 'b': true,
}

var grepLongBools = map[string]bool{
	"--ignore-case":        true,
	"--extended-regexp":    true,
	"--fixed-strings":      true,
	"--invert-match":       true,
	"--count":              true,
	"--files-with-matches": true,
	"--line-number":        true,
	"--with-filename":      true,
	"--no-filename":        true,
	"--only-matching":      true,
	"--word-regexp":        true,
	"--line-regexp":        true,
	"--quiet":              true,
	"--no-messages":        true,
}

func validateGrep(args []string) error {
	spec := shortSpec{bools: grepBools}
	positionals := 0
	for i := 0; i < len(args); {
		tok := args[i]
		switch {
		case tok == "--":
			return unsupportedf("grep 不接受 -- 之后的位置参数")
		case strings.HasPrefix(tok, "--"):
			if strings.Contains(tok, "=") || !grepLongBools[tok] {
				return unsupportedf("grep 参数 %q 不在只读自动执行集合内", tok)
			}
			i++
		case len(tok) > 1 && tok[0] == '-':
			next, err := spec.parse(args, i)
			if err != nil {
				return err
			}
			i = next
		default:
			positionals++
			i++
		}
	}
	if positionals == 0 {
		return unsupportedf("grep 缺少模式参数")
	}
	if positionals > 1 {
		return unsupportedf("grep 不接受文件参数；只处理管道输入")
	}
	return nil
}

// head/tail 只允许 -n N / --lines N 形式，N 有明确上限。
func validateHeadTail(name string, args []string) error {
	for i := 0; i < len(args); i++ {
		tok := args[i]
		switch {
		case tok == "-n" || tok == "--lines":
			i++
			if i >= len(args) {
				return unsupportedf("%s 的 %s 缺少取值", name, tok)
			}
			if err := validateLineCount(args[i]); err != nil {
				return err
			}
		case strings.HasPrefix(tok, "--lines="):
			if err := validateLineCount(strings.TrimPrefix(tok, "--lines=")); err != nil {
				return err
			}
		case strings.HasPrefix(tok, "-n") && !strings.HasPrefix(tok, "--") && len(tok) > 2:
			if err := validateLineCount(tok[2:]); err != nil {
				return err
			}
		case isObsoleteCount(tok):
			if err := validateLineCount(tok[1:]); err != nil {
				return err
			}
		default:
			return unsupportedf("%s 参数 %q 不在只读自动执行集合内", name, tok)
		}
	}
	return nil
}

// isObsoleteCount 识别 head -20 这种 POSIX 旧式行数写法。
func isObsoleteCount(tok string) bool {
	if len(tok) < 2 || tok[0] != '-' {
		return false
	}
	for _, r := range tok[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

const maxFilterLines = 1000

func validateLineCount(value string) error {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || n < 1 || n > maxFilterLines {
		return unsupportedf("行数上限必须在 1 到 %d 之间", maxFilterLines)
	}
	return nil
}

// sort 只允许排序语义选项；-o/--output 会写文件，必须拒绝。
var sortBools = map[byte]bool{'n': true, 'r': true, 'u': true, 'h': true, 'g': true, 'b': true, 'f': true}

var sortListPattern = regexp.MustCompile(`^[0-9]+([.,][0-9]*)?[a-zA-Z,]*$`)

func validateSort(args []string) error {
	spec := shortSpec{
		bools:  sortBools,
		valued: map[byte]bool{'k': true, 't': true},
		onValue: func(c byte, value string) error {
			switch c {
			case 'k':
				if !sortListPattern.MatchString(value) {
					return unsupportedf("sort 的 -k 取值 %q 不受支持", value)
				}
			case 't':
				if len([]rune(value)) != 1 {
					return unsupportedf("sort 的 -t 只接受单个分隔字符")
				}
			}
			return nil
		},
	}
	for i := 0; i < len(args); {
		tok := args[i]
		switch {
		case strings.HasPrefix(tok, "--"):
			name, value, hasValue := strings.Cut(tok, "=")
			switch name {
			case "--numeric-sort", "--reverse", "--unique", "--human-numeric-sort",
				"--general-numeric-sort", "--ignore-leading-blanks", "--ignore-case":
				if hasValue {
					return unsupportedf("sort 布尔参数 %q 不接受取值", name)
				}
			case "--key", "--field-separator":
				if !hasValue {
					i++
					if i >= len(args) {
						return unsupportedf("sort 参数 %q 缺少取值", name)
					}
					value = args[i]
				}
				if value == "" {
					return unsupportedf("sort 参数 %q 的取值为空", name)
				}
			default:
				return unsupportedf("sort 参数 %q 不在只读自动执行集合内", name)
			}
			i++
		case len(tok) > 1 && tok[0] == '-':
			next, err := spec.parse(args, i)
			if err != nil {
				return err
			}
			i = next
		default:
			return unsupportedf("sort 不接受位置参数（文件输入）")
		}
	}
	return nil
}

var uniqBools = map[byte]bool{'c': true, 'd': true, 'u': true, 'i': true}

func validateUniq(args []string) error {
	spec := shortSpec{bools: uniqBools}
	for i := 0; i < len(args); {
		tok := args[i]
		switch {
		case strings.HasPrefix(tok, "--"):
			switch tok {
			case "--count", "--repeated", "--unique", "--ignore-case":
				i++
			default:
				return unsupportedf("uniq 参数 %q 不在只读自动执行集合内", tok)
			}
		case len(tok) > 1 && tok[0] == '-':
			next, err := spec.parse(args, i)
			if err != nil {
				return err
			}
			i = next
		default:
			return unsupportedf("uniq 不接受位置参数（文件输入）")
		}
	}
	return nil
}

var wordCountBools = map[byte]bool{'l': true, 'w': true, 'c': true, 'm': true, 'L': true}

func validateWordCount(args []string) error {
	spec := shortSpec{bools: wordCountBools}
	for i := 0; i < len(args); {
		tok := args[i]
		switch {
		case strings.HasPrefix(tok, "--"):
			switch tok {
			case "--lines", "--words", "--bytes", "--chars", "--max-line-length":
				i++
			default:
				return unsupportedf("wc 参数 %q 不在只读自动执行集合内", tok)
			}
		case len(tok) > 1 && tok[0] == '-':
			next, err := spec.parse(args, i)
			if err != nil {
				return err
			}
			i = next
		default:
			return unsupportedf("wc 不接受位置参数（文件输入）")
		}
	}
	return nil
}

var cutListPattern = regexp.MustCompile(`^[0-9]+(-[0-9]*)?(,[0-9]+(-[0-9]*)?)*$`)

func validateCut(args []string) error {
	sawList := false
	checkList := func(value string) error {
		if !cutListPattern.MatchString(value) {
			return unsupportedf("cut 的位置列表 %q 不受支持", value)
		}
		sawList = true
		return nil
	}
	checkDelimiter := func(value string) error {
		if len([]rune(value)) != 1 {
			return unsupportedf("cut 的 -d 只接受单个分隔字符")
		}
		return nil
	}
	spec := shortSpec{
		bools:  map[byte]bool{'s': true, 'n': true, 'z': true},
		valued: map[byte]bool{'f': true, 'c': true, 'b': true, 'd': true},
		onValue: func(c byte, value string) error {
			if c == 'd' {
				return checkDelimiter(value)
			}
			return checkList(value)
		},
	}
	for i := 0; i < len(args); {
		tok := args[i]
		switch {
		case strings.HasPrefix(tok, "--"):
			name, value, hasValue := strings.Cut(tok, "=")
			switch name {
			case "--fields", "--characters", "--bytes", "--delimiter":
				if !hasValue {
					i++
					if i >= len(args) {
						return unsupportedf("cut 参数 %q 缺少取值", name)
					}
					value = args[i]
				}
				var err error
				if name == "--delimiter" {
					err = checkDelimiter(value)
				} else {
					err = checkList(value)
				}
				if err != nil {
					return err
				}
			case "--complement", "--only-delimited", "--zero-terminated":
				if hasValue {
					return unsupportedf("cut 布尔参数 %q 不接受取值", name)
				}
			default:
				return unsupportedf("cut 参数 %q 不在只读自动执行集合内", name)
			}
			i++
		case len(tok) > 1 && tok[0] == '-':
			next, err := spec.parse(args, i)
			if err != nil {
				return err
			}
			i = next
		default:
			return unsupportedf("cut 不接受位置参数（文件输入）")
		}
	}
	if !sawList {
		return unsupportedf("cut 需要 -f/-c/-b 指定字段列表")
	}
	return nil
}

var trBools = map[byte]bool{'d': true, 's': true, 'c': true, 'C': true}

func validateTr(args []string) error {
	spec := shortSpec{bools: trBools}
	positionals := 0
	for i := 0; i < len(args); {
		tok := args[i]
		switch {
		case strings.HasPrefix(tok, "--"):
			switch tok {
			case "--delete", "--squeeze-repeats", "--complement", "--truncate-set1":
				i++
			default:
				return unsupportedf("tr 参数 %q 不在只读自动执行集合内", tok)
			}
		case len(tok) > 1 && tok[0] == '-':
			next, err := spec.parse(args, i)
			if err != nil {
				return err
			}
			i = next
		default:
			if strings.ContainsRune(tok, 0) || len(tok) > 256 {
				return unsupportedf("tr 的字符集参数不合法")
			}
			positionals++
			i++
		}
	}
	if positionals < 1 || positionals > 2 {
		return unsupportedf("tr 需要一到两个字符集参数")
	}
	return nil
}
