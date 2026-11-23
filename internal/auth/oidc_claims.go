// Copyright 2017-2018 The Argo Authors
// SPDX-License-Identifier: Apache-2.0
// Adapted from Argo CD util/oidc/oidc.go. AegisOps requests only essential ID token claims.
package auth

import (
	"encoding/json"
	"golang.org/x/oauth2"
)

type essentialClaim struct {
	Essential bool `json:"essential"`
}
type ClaimsRequest struct {
	IDToken map[string]essentialClaim `json:"id_token"`
}

func createClaimsAuthenticationRequestParameter(requestedClaims map[string]essentialClaim) (oauth2.AuthCodeOption, error) {
	claimsRequest := ClaimsRequest{IDToken: requestedClaims}
	claimsRequestRAW, err := json.Marshal(claimsRequest)
	if err != nil {
		return nil, err
	}
	return oauth2.SetAuthURLParam("claims", string(claimsRequestRAW)), nil
}

func GetScopesOrDefault(scopes []string) []string {
	if len(scopes) == 0 {
		return []string{"openid", "profile", "email", "groups"}
	}
	return scopes
}

// 以下 claim 口径抄自 Dex connector/oidc（github.com/dexidp/dex，Apache-2.0，
// connector/oidc/oidc.go 的 getUserInfo）：登录名默认取 preferred_username；配了
// 覆盖键时，默认只在 preferred_username 缺失时才改用覆盖键，overrideClaimMapping
// 为真则无条件用覆盖键；仍取不到就退回 name（Dex 的 userNameKey 默认值），再退回
// email（Dex 在 preferred_username 为空时给用户名兜底的字段）。
const (
	// defaultPreferredUsernameClaim 是 Dex PreferredUsernameKey 的默认值。
	defaultPreferredUsernameClaim = "preferred_username"
	// defaultNameClaim 是 Dex userNameKey 的默认值。
	defaultNameClaim = "name"
	// defaultEmailClaim 是 Dex claimMappings.email 的默认值。
	defaultEmailClaim = "email"
)

// preferredLoginName 按上述口径挑本次登录的登录名。claim 读取 id_token 里的字符串
// claim，缺失返回空串；override 对应 Dex 的 claimMappings.preferred_username，
// overrideClaim 对应 Dex 的 overrideClaimMapping。
func preferredLoginName(claim func(string) string, override string, overrideClaim bool) string {
	name := claim(defaultPreferredUsernameClaim)
	if (name == "" || overrideClaim) && override != "" {
		if v := claim(override); v != "" || overrideClaim {
			name = v
		}
	}
	if name == "" {
		name = claim(defaultNameClaim)
	}
	if name == "" {
		name = claim(defaultEmailClaim)
	}
	return name
}
