package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

const (
	RoleProvider = "provider"
	RoleInternal = "internal"
)

var ErrUnauthenticated = errors.New("unauthenticated")

type Principal struct {
	Subject    string
	ProviderID string
	Roles      []string
}

func (p Principal) HasRole(role string) bool {
	return slices.Contains(p.Roles, role)
}

func (p Principal) IsInternal() bool { return p.HasRole(RoleInternal) }

func (p Principal) IsProvider() bool {
	return p.HasRole(RoleProvider) && p.ProviderID != ""
}

func (p Principal) CanAccessProvider(providerID string) bool {
	if providerID == "" {
		return false
	}

	return p.IsInternal() || (p.IsProvider() && p.ProviderID == providerID)
}

type Authenticator interface {
	Authenticate(ctx context.Context, rawToken string) (Principal, error)
}

type OIDCAuthenticator struct {
	verifier *oidc.IDTokenVerifier
}

func NewOIDCAuthenticator(ctx context.Context, issuer, jwksURL, audience string) (*OIDCAuthenticator, error) {
	if issuer == "" || jwksURL == "" || audience == "" {
		return nil, errors.New("auth: issuer, JWKS URL and audience are required")
	}

	keySet := oidc.NewRemoteKeySet(ctx, jwksURL)

	return &OIDCAuthenticator{
		verifier: oidc.NewVerifier(issuer, keySet, &oidc.Config{ClientID: audience}),
	}, nil
}

func (a *OIDCAuthenticator) Authenticate(ctx context.Context, rawToken string) (Principal, error) {
	if strings.TrimSpace(rawToken) == "" {
		return Principal{}, ErrUnauthenticated
	}

	token, err := a.verifier.Verify(ctx, rawToken)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}

	var claims struct {
		ProviderID  string `json:"provider_id"`
		RealmAccess struct {
			Roles []string `json:"roles"`
		} `json:"realm_access"`
	}

	if err := token.Claims(&claims); err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}

	return Principal{
		Subject:    token.Subject,
		ProviderID: claims.ProviderID,
		Roles:      claims.RealmAccess.Roles,
	}, nil
}

type principalKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)

	return p, ok
}
