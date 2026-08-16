package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// Fixtures for scoped tokens and the audit log (spec section 10). Like every
// other shape in this suite they are hand-written rather than shared with the
// hub, so a change in the reference implementation cannot quietly change the
// contract being graded.

type tokenRecord struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	CreatedAt string   `json:"createdAt"`
	CreatedBy string   `json:"createdBy"`
	ExpiresAt *string  `json:"expiresAt"`
	RevokedAt *string  `json:"revokedAt"`
}

type mintedToken struct {
	Token tokenRecord `json:"token"`
	// Secret is returned by the mint response and never again.
	Secret string `json:"secret"`
}

// mintToken creates a scoped Admin API token with the suite's own credential.
func (e Env) mintToken(ctx context.Context, name string, scopes ...string) (mintedToken, error) {
	var minted mintedToken
	err := e.expect(ctx, http.MethodPost, "/api/v1/tokens", e.AdminToken,
		map[string]any{"name": name, "scopes": scopes}, http.StatusCreated, &minted)
	if err != nil {
		return mintedToken{}, err
	}
	if minted.Secret == "" {
		return mintedToken{}, fmt.Errorf("mint %q: response carried no secret", name)
	}
	if minted.Token.ID == "" {
		return mintedToken{}, fmt.Errorf("mint %q: response carried no token id", name)
	}
	return minted, nil
}

func (e Env) listTokens(ctx context.Context) ([]tokenRecord, error) {
	var listed struct {
		Tokens []tokenRecord `json:"tokens"`
	}
	err := e.expect(ctx, http.MethodGet, "/api/v1/tokens", e.AdminToken,
		nil, http.StatusOK, &listed)
	return listed.Tokens, err
}

type auditRecord struct {
	ID            string          `json:"id"`
	At            string          `json:"at"`
	TokenID       string          `json:"tokenId"`
	TokenName     string          `json:"tokenName"`
	Method        string          `json:"method"`
	Path          string          `json:"path"`
	Status        int             `json:"status"`
	SourceIP      string          `json:"sourceIp"`
	PayloadDigest string          `json:"payloadDigest"`
	ServerID      string          `json:"serverId"`
	Detail        json.RawMessage `json:"detail"`
}

type auditPage struct {
	Records    []auditRecord `json:"records"`
	NextCursor string        `json:"nextCursor"`
}

// auditRecords reads one page of the audit log.
func (e Env) auditRecords(ctx context.Context, parameters url.Values) (auditPage, error) {
	path := "/api/v1/audit"
	if encoded := parameters.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var page auditPage
	err := e.expect(ctx, http.MethodGet, path, e.AdminToken, nil, http.StatusOK, &page)
	return page, err
}

// refused asserts that a request made with a scoped token is answered 403 with
// the protocol's forbidden code, which is what separates "your token may not"
// from "no such thing" (spec section 2.2).
func (e Env) refused(ctx context.Context, method, path, bearer string, body any, why string) error {
	if err := e.expectError(ctx, method, path, bearer, body, http.StatusForbidden, "forbidden"); err != nil {
		return fmt.Errorf("%s: %w", why, err)
	}
	return nil
}
