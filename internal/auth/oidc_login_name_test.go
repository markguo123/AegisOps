// 登录名 claim 口径用例，对应 Dex connector/oidc 的 getUserInfo（Apache-2.0）。
package auth

import "testing"

func TestPreferredLoginName(t *testing.T) {
	for _, tc := range []struct {
		name          string
		claims        map[string]string
		override      string
		overrideClaim bool
		want          string
	}{
		{
			name:   "标准 IdP 取 preferred_username",
			claims: map[string]string{"preferred_username": "p6070718", "name": "张三", "email": "p6070718@example.com"},
			want:   "p6070718",
		},
		{
			name:   "Keycloak 一类 name 为空的 IdP 落到 preferred_username",
			claims: map[string]string{"preferred_username": "alice", "email": "alice@example.com"},
			want:   "alice",
		},
		{
			name:   "没有 preferred_username 时退回 name（Dex userNameKey 默认值）",
			claims: map[string]string{"name": "Alice", "email": "alice@example.com"},
			want:   "Alice",
		},
		{
			name:   "两者都没有时退回 email",
			claims: map[string]string{"email": "alice@example.com", "sub": "abc"},
			want:   "alice@example.com",
		},
		{
			name:   "覆盖键只在默认 claim 缺失时生效（Dex overrideClaimMapping 默认 false）",
			claims: map[string]string{"preferred_username": "p6070718", "sAMAccountName": "P6070718"},
			override: "sAMAccountName",
			want:   "p6070718",
		},
		{
			name:     "overrideClaimMapping 打开后无条件用覆盖键",
			claims:   map[string]string{"preferred_username": "张三", "name": "p6070718"},
			override: "name",
			overrideClaim: true,
			want:     "p6070718",
		},
		{
			name:     "覆盖键本身缺失时继续按默认链退回",
			claims:   map[string]string{"name": "Alice"},
			override: "sAMAccountName",
			want:     "Alice",
		},
		{
			name:     "覆盖键打开且覆盖键缺失时用覆盖键结果（不回退默认 claim）",
			claims:   map[string]string{"preferred_username": "p6070718", "email": "p@example.com"},
			override: "sAMAccountName",
			overrideClaim: true,
			want:     "p@example.com",
		},
		{
			name:   "全空时返回空串，由建档方兜底",
			claims: map[string]string{"sub": "abc"},
			want:   "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claim := func(k string) string { return tc.claims[k] }
			if got := preferredLoginName(claim, tc.override, tc.overrideClaim); got != tc.want {
				t.Fatalf("preferredLoginName() = %q, want %q", got, tc.want)
			}
		})
	}
}
