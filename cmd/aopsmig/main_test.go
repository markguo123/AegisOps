package main

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k8s-ai/k8s-ai/internal/cluster"
)

func randKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return k
}

func mustCipher(t *testing.T, key []byte) *cluster.Cipher {
	t.Helper()
	c, err := cluster.NewCipher(key)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return c
}

func writeB64(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.TrimSpace(content)+"\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestClassifyAndMigrate(t *testing.T) {
	dir := t.TempDir()
	newKey := randKey(t)
	oldKey := randKey(t)
	cls := classifiers{newC: mustCipher(t, newKey), oldC: mustCipher(t, oldKey)}

	// new-key readable → status "new"
	newCipher, err := cls.newC.Encrypt([]byte("api key value"))
	if err != nil {
		t.Fatalf("encrypt new: %v", err)
	}
	// old-key-only readable → status "old"
	oldCipher, err := cls.oldC.Encrypt([]byte("kubeconfig secret body"))
	if err != nil {
		t.Fatalf("encrypt old: %v", err)
	}
	// both unreadable
	bothCipher := "v1:" + "bG9uZy1nYXJiYWdlLWNpcGhlcnRleHQtbWFkZWF2YWlsYWJsZQ=="

	writeB64(t, dir, "credential_abc.b64", newCipher)
	writeB64(t, dir, "setting_model__api_key.b64", oldCipher)
	writeB64(t, dir, "credential_bad.b64", bothCipher)

	// scan & classify
	outs, err := scan(dir, cls)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(outs) != 3 {
		t.Fatalf("expect 3 records, got %d", len(outs))
	}
	byName := map[string]string{}
	for _, o := range outs {
		byName[o.name] = o.status
	}
	if byName["credential_abc"] != "new" {
		t.Errorf("credential_abc: want new, got %s", byName["credential_abc"])
	}
	if byName["setting_model__api_key"] != "old" {
		t.Errorf("setting_model__api_key: want old, got %s", byName["setting_model__api_key"])
	}
	if byName["credential_bad"] != "both" {
		t.Errorf("credential_bad: want both, got %s", byName["credential_bad"])
	}

	// check with unreadable row → exitUnreadable
	code, err := check(dir, cls, &bytes.Buffer{})
	if err == nil || code != exitUnreadable {
		t.Fatalf("check with bad row: want exitUnreadable, got code=%d err=%v", code, err)
	}

	// migrate with bad row → refuses entirely (no .new written)
	outDir2 := filepath.Join(dir, "out2")
	code, err = migrate(dir, cls, "", outDir2, &bytes.Buffer{})
	if err == nil || code != exitUnreadable {
		t.Fatalf("migrate with bad row: want exitUnreadable, got code=%d err=%v", code, err)
	}
	if _, serr := os.Stat(outDir2); !os.IsNotExist(serr) {
		t.Fatal("migrate should not have created output dir when refusing")
	}

	// now remove the bad record and migrate
	if err := os.Remove(filepath.Join(dir, "credential_bad.b64")); err != nil {
		t.Fatalf("remove bad: %v", err)
	}
	code, err = migrate(dir, cls, "", "", &bytes.Buffer{})
	if err != nil || code != 0 {
		t.Fatalf("migrate: want 0, got code=%d err=%v", code, err)
	}
	kcNew := readB64(t, filepath.Join(dir, "setting_model__api_key.new"))
	plain, err := cls.newC.Decrypt(kcNew)
	if err != nil {
		t.Fatalf("decrypt migrated with new key: %v", err)
	}
	if string(plain) != "kubeconfig secret body" {
		t.Fatalf("migrated plaintext mismatch: %q", plain)
	}
	// the already-new row must not have a .new sibling
	if _, serr := os.Stat(filepath.Join(dir, "credential_abc.new")); !os.IsNotExist(serr) {
		t.Fatal("already-new row should not be rewritten to .new")
	}
}

func readB64(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.TrimSpace(string(b))
}

func TestMigrateAdminHash(t *testing.T) {
	dir := t.TempDir()
	cls := classifiers{
		newC: mustCipher(t, randKey(t)),
		oldC: mustCipher(t, randKey(t)),
	}
	oldCipher, err := cls.oldC.Encrypt([]byte("whatever"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	writeB64(t, dir, "credential_x.b64", oldCipher)

	code, err := migrate(dir, cls, "s3cretAdminPw", "", &bytes.Buffer{})
	if err != nil || code != 0 {
		t.Fatalf("migrate: code=%d err=%v", code, err)
	}
	h := strings.TrimSpace(readB64(t, filepath.Join(dir, "admin.hash")))
	// HashPassword 与 Server 同源，这里只验格式；完整 verify 由 internal/auth 单测覆盖。
	if !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("admin.hash 格式异常: %q", h)
	}
	if len(strings.Split(h, "$")) != 6 {
		t.Fatalf("admin.hash 段数异常: %q", h)
	}
}
