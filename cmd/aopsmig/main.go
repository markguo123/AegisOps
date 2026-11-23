// Command aopsmig 是「加密密钥轮换后存量密文不可读」场景的迁移/一致性工具
// （P6.12：敏感值只用 AEGISOPS_ENC_KEY 加密落库）。
//
// 背景：AES-256-GCM 密文只能用加密它的那把密钥解开。如果平台凭证密钥被轮换
// （Secret 换新 key），而数据库里已经是旧 key 加密的存量密文，Server 启动后
// 会出现：admin 口令哈希读不出（登录 401）、集群 kubeconfig 解不开（集群报错）、
// settings 里的 api_key 等读不出（聊天/模型不可用）。
//
// 这个工具不复刻 Server 逻辑，而是直接复用 internal/cluster 与 internal/auth：
// 新旧密钥都走 cluster.NewCipherFromKeyString（同一份 decodeCipherKey），
// admin 口令走 auth.HashPassword，保证对外加解密口径与 Server 完全一致。
// 它只做文件级迁移：不改数据库、不写审计，输出 .new 文件由编排脚本应用。
//
// 用法：
//
//	aopsmig check   <workdir> 只读检查
//	aopsmig migrate <workdir> 生成重加密后的 .new 文件
//
// workdir 里放 N 个 "<名字>.b64" 文件，每个文件内容是 `v1:` 前缀的 base64 密文
// （文件名建议沿用导出时的记录名，例如 credentials.id 或 settings 的
// group__key，便于编排脚本回写出准确的 UPDATE）。migrate 会为每个"旧 key 可解、
// 新 key 不可解"的文件生成同名 "<名字>.new"；任一密文新旧两个 key 都解不开时
// 整体拒绝，绝不落不确定的结果。
//
// 环境变量：
//
//	AEGISOPS_ENC_KEY   当前（新）密钥，64 位 hex 或 base64 32 字节
//	AEGISOPS_KEY_OLD   被轮换掉的旧密钥，同上；migrate 必须提供
//	AEGISOPS_ADMIN_PASSWORD  可选，提供时额外生成 admin.hash（argon2id）
package main

import (
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/k8s-ai/k8s-ai/internal/auth"
	"github.com/k8s-ai/k8s-ai/internal/cluster"
)

// exitUnreadable 表示存在新旧密钥都解不开的密文（需人工核对密钥来源）。
const exitUnreadable = 3

// exitNeedsMigrate 表示检查通过、但存在"旧 key 可解、需迁移"的记录。
const exitNeedsMigrate = 2

func usage() {
	fmt.Fprintf(os.Stderr, `aopsmig: 加密密钥轮换后存量密文迁移工具

用法:
  aopsmig check   <workdir>
  aopsmig migrate <workdir> [--admin-password=...] [--out=...]

环境变量:
  AEGISOPS_ENC_KEY         当前(新)密钥: 64位hex 或 base64 32字节
  AEGISOPS_KEY_OLD         被轮换掉的旧密钥, migrate 必须提供
  AEGISOPS_ADMIN_PASSWORD  可选, 提供时生成 admin.hash (argon2id)

退出码:
  check   : 0=全部已由新key可解; %d=需迁移; %d=存在双不可解
  migrate : 0=完成; %d=解锁失败(取消全部)
`, exitNeedsMigrate, exitUnreadable, exitUnreadable)
}

// outcome 是单条记录的检查结果。
type outcome struct {
	name   string
	cipher string
	// status: "new"=新 key 可解(无需迁移) | "old"=仅旧 key 可解(需迁移)
	//        | "both"=新旧都不可解(需人工核对密钥)
	status  string
	oldPath string // 原 .b64 绝对路径
	newPath string // migrate 后 .new 绝对路径
}

// classifiers 收拢新旧 Cipher，提供一个按密文分类的入口（便于单测）。
type classifiers struct {
	newC *cluster.Cipher
	oldC *cluster.Cipher
}

// classify 返回密文的状态：new / old / both。
func (c classifiers) classify(encoded string) string {
	if _, err := c.newC.Decrypt(encoded); err == nil {
		return "new"
	}
	if _, err := c.oldC.Decrypt(encoded); err == nil {
		return "old"
	}
	return "both"
}

// scan 遍历 workdir 里的 *.b64 文件并逐条分类（只读）。
func scan(dir string, cls classifiers) ([]outcome, error) {
	var outs []outcome
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".b64") {
			name := strings.TrimSuffix(filepath.Base(path), ".b64")
			raw, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			cipher := strings.TrimSpace(string(raw))
			if cipher == "" {
				return fmt.Errorf("%s: 空密文，跳过该记录（或密钥字段本身为空）", path)
			}
			outs = append(outs, outcome{
				name: name, cipher: cipher, status: cls.classify(cipher),
				oldPath: path, newPath: filepath.Join(dir, name+".new"),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(outs, func(i, j int) bool { return outs[i].name < outs[j].name })
	return outs, nil
}

// check 只读地报告每条记录的状态，并给出建议退出码。
func check(dir string, cls classifiers, w io.Writer) (int, error) {
	outs, err := scan(dir, cls)
	if err != nil {
		return 1, err
	}
	if len(outs) == 0 {
		fmt.Fprintln(w, "no .b64 records found (nothing to check)")
		return 0, nil
	}
	var nNew, nOld, nBoth int
	for _, o := range outs {
		switch o.status {
		case "new":
			nNew++
		case "old":
			nOld++
		default:
			nBoth++
		}
		fmt.Fprintf(w, "  %-8s %s (len=%d)\n", o.status, o.name, len(o.cipher))
	}
	fmt.Fprintf(w, "summary: new=%d needs_migrate=%d unreadable=%d\n", nNew, nOld, nBoth)
	if nBoth > 0 {
		return exitUnreadable, errors.New("存在新旧密钥都解不开的密文，请核对 AEGISOPS_KEY_OLD 是否为轮换前的真正密钥")
	}
	if nOld > 0 {
		return exitNeedsMigrate, nil
	}
	return 0, nil
}

// migrate 生成重加密的 .new 文件；任何双不可解记录都整体中止、不写任何输出。
func migrate(dir string, cls classifiers, adminPass, outDir string, w io.Writer) (int, error) {
	outs, err := scan(dir, cls)
	if err != nil {
		return 1, err
	}
	// 先整体判定：任何双不可解记录都必须在上手写文件之前止住，避免半途中止留下
	// 一部分重加密、一部分原样的不可预期状态。
	for _, o := range outs {
		if o.status == "both" {
			return exitUnreadable, fmt.Errorf("%s: 新旧密钥都解不开，已取消全部迁移（不写入任何 .new）", o.name)
		}
	}
	if outDir == "" {
		outDir = dir
	} else if err := os.MkdirAll(outDir, 0o755); err != nil {
		return 1, fmt.Errorf("create out dir: %w", err)
	}
	var nNew, nMigrated int
	for _, o := range outs {
		if o.status == "new" {
			nNew++
			continue
		}
		plain, derr := cls.oldC.Decrypt(o.cipher)
		if derr != nil {
			return exitUnreadable, fmt.Errorf("%s: 旧密钥解密失败: %w", o.name, derr)
		}
		enc, eerr := cls.newC.Encrypt(plain)
		if eerr != nil {
			return 1, fmt.Errorf("%s: 新密钥重加密失败: %w", o.name, eerr)
		}
		// 回读验证，确保写出去的就是能解回同明文的密文。
		rt, rerr := cls.newC.Decrypt(enc)
		if rerr != nil || subtle.ConstantTimeCompare(rt, plain) != 1 {
			return 1, fmt.Errorf("%s: 重加密回读校验失败: %v", o.name, rerr)
		}
		dest := filepath.Join(outDir, o.name+".new")
		if werr := os.WriteFile(dest, []byte(enc+"\n"), 0o600); werr != nil {
			return 1, fmt.Errorf("%s: 写 %s: %w", o.name, dest, werr)
		}
		fmt.Fprintf(w, "migrated  %s (len %d -> %d)\n", o.name, len(o.cipher), len(enc))
		nMigrated++
	}

	if adminPass != "" {
		h, herr := auth.HashPassword(adminPass)
		if herr != nil {
			return 1, fmt.Errorf("生成 admin 口令哈希失败: %w", herr)
		}
		if werr := os.WriteFile(filepath.Join(outDir, "admin.hash"), []byte(h+"\n"), 0o600); werr != nil {
			return 1, fmt.Errorf("写 admin.hash: %w", werr)
		}
		fmt.Fprintf(w, "wrote     admin.hash (len=%d)\n", len(h))
	}

	fmt.Fprintf(w, "summary: already_new=%d migrated=%d\n", nNew, nMigrated)
	if nMigrated == 0 {
		fmt.Fprintln(w, "nothing to migrate (all ciphertexts already new-key readable)")
	}
	return 0, nil
}

func main() {
	if len(os.Args) < 3 {
		usage()
		os.Exit(1)
	}
	mode := os.Args[1]
	if mode != "check" && mode != "migrate" {
		fmt.Fprintf(os.Stderr, "unknown mode: %s\n", mode)
		usage()
		os.Exit(1)
	}
	fs := flag.NewFlagSet(mode, flag.ExitOnError)
	var outDir string
	var adminPass string
	fs.StringVar(&outDir, "out", "", "migrate 输出目录（默认与 workdir 相同）")
	fs.StringVar(&adminPass, "admin-password", "", "可选：生成 admin.hash (argon2id)（默认取 AEGISOPS_ADMIN_PASSWORD）")
	_ = fs.Parse(os.Args[2:])
	dir := fs.Arg(0)
	if dir == "" {
		usage()
		os.Exit(1)
	}

	newHex := os.Getenv("AEGISOPS_ENC_KEY")
	oldHex := os.Getenv("AEGISOPS_KEY_OLD")
	if adminPass == "" {
		adminPass = os.Getenv("AEGISOPS_ADMIN_PASSWORD")
	}
	if strings.TrimSpace(newHex) == "" {
		if mode == "migrate" {
			fmt.Fprintln(os.Stderr, "migrate: 需要 AEGISOPS_ENC_KEY（当前/新密钥）")
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "check: 需要 AEGISOPS_ENC_KEY（当前/新密钥），无法检查")
		os.Exit(1)
	}
	if mode == "migrate" && strings.TrimSpace(oldHex) == "" {
		fmt.Fprintln(os.Stderr, "migrate: 需要 AEGISOPS_KEY_OLD（被轮换掉的旧密钥）")
		os.Exit(1)
	}

	newC, err := cluster.NewCipherFromKeyString(newHex)
	if err != nil {
		fmt.Fprintf(os.Stderr, "new key: %v\n", err)
		os.Exit(1)
	}
	oldC, err := cluster.NewCipherFromKeyString(oldHex)
	if err != nil {
		fmt.Fprintf(os.Stderr, "old key: %v\n", err)
		os.Exit(1)
	}
	cls := classifiers{newC: newC, oldC: oldC}

	var code int
	switch mode {
	case "check":
		code, err = check(dir, cls, os.Stdout)
	case "migrate":
		code, err = migrate(dir, cls, adminPass, outDir, os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(code)
	}
	os.Exit(code)
}
