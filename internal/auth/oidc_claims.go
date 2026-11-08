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
