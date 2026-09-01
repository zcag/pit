package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

// This is a port of another project's MCP resource-server layer (internal/api/oauth.go),
// generalised: nothing here knows what pit is, so when a third service wants a
// hosted connector this file becomes a shared module and both import it. Kept
// in-tree for now rather than extracting speculatively — the extraction is
// worth doing once there are two proven consumers, not before.

// WellKnownPRM is the RFC 9728 Protected Resource Metadata base path. Claude and
// ChatGPT probe both this and the path-scoped "<base><resource path>" variant,
// so both are served.
const WellKnownPRM = "/.well-known/oauth-protected-resource"

// OAuth is the OAuth 2.1 resource-server config for /mcp. It lets the endpoint
// accept authorization-server-issued JWTs (the Claude.ai / ChatGPT "Connect"
// flow) alongside the static API key, and serves the metadata that bootstraps
// that flow.
//
// nil means disabled: /mcp stays API-key-only and advertises no OAuth, so this
// whole layer is inert until the issuer and resource are configured.
type OAuth struct {
	name     string // log prefix
	issuer   string // e.g. https://x.authkit.app
	resource string // the aud we enforce: this MCP endpoint's public URL
	keyfunc  keyfunc.Keyfunc

	// allowed gates the sub and email claims. Empty means any valid token from
	// the issuer passes; non-empty means sub OR email must be listed. pit is
	// single-user, so identity is the gate — a shared AuthKit app would
	// otherwise trust every account it holds.
	allowed     map[string]bool
	logIdentity sync.Once
}

// LoadOAuth builds the config, or returns nil when issuer/resource are empty.
// ctx bounds the background JWKS refresh. allowedEmails/allowedSubs are
// comma-separated; which claim an access token actually carries varies by
// authorization-server config, so a match on either is accepted.
func LoadOAuth(ctx context.Context, name, issuer, resource, allowedEmails, allowedSubs string) *OAuth {
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	resource = strings.TrimSpace(resource)
	if issuer == "" || resource == "" {
		return nil
	}
	kf, err := keyfunc.NewDefaultCtx(ctx, []string{issuer + "/oauth2/jwks"})
	if err != nil {
		log.Printf("%s: mcp oauth init failed — OAuth disabled, MCP stays API-key-only: %v", name, err)
		return nil
	}
	allowed := map[string]bool{}
	for _, e := range strings.Split(allowedEmails, ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			allowed[e] = true
		}
	}
	for _, s := range strings.Split(allowedSubs, ",") {
		if s = strings.TrimSpace(s); s != "" {
			allowed[s] = true
		}
	}
	gate := "open (any valid token)"
	if len(allowed) > 0 {
		gate = "allowlist"
	}
	log.Printf("%s: mcp oauth enabled (issuer=%s resource=%s gate=%s)", name, issuer, resource, gate)
	return &OAuth{name: name, issuer: issuer, resource: resource, keyfunc: kf, allowed: allowed}
}

// Verify validates an access token against the JWKS, enforcing issuer, audience
// (== resource, the RFC 8707 replay defence), signing algorithm, a small clock
// skew allowance, and the optional identity allowlist.
func (o *OAuth) Verify(token string) bool {
	claims := jwt.MapClaims{}
	tok, err := jwt.ParseWithClaims(token, claims, o.keyfunc.Keyfunc,
		jwt.WithIssuer(o.issuer),
		jwt.WithAudience(o.resource),
		jwt.WithValidMethods([]string{"RS256", "ES256"}),
		jwt.WithLeeway(60*time.Second),
	)
	if err != nil || !tok.Valid {
		return false
	}
	sub, _ := claims["sub"].(string)
	email, _ := claims["email"].(string)
	// Surface the real identity once, so the allowlist can be pinned to the exact
	// claim this authorization server actually issues.
	o.logIdentity.Do(func() {
		log.Printf("%s: oauth first verified-token identity (sub=%q email=%q)", o.name, sub, email)
	})
	if len(o.allowed) == 0 {
		return true
	}
	if (sub != "" && o.allowed[sub]) || (email != "" && o.allowed[strings.ToLower(email)]) {
		return true
	}
	log.Printf("%s: oauth token rejected — identity not allowlisted (sub=%q email=%q)", o.name, sub, email)
	return false
}

// prmJSON is the Protected Resource Metadata document (RFC 9728).
//
// offline_access is advertised so the client asks for it and the authorization
// server issues a refresh token. Without it the access token silently expires
// and the host 401s with no way to refresh, which reads to the user as the
// connector randomly breaking.
func (o *OAuth) prmJSON() []byte {
	b, _ := json.Marshal(map[string]any{
		"resource":                 o.resource,
		"authorization_servers":    []string{o.issuer},
		"scopes_supported":         []string{"openid", "email", "offline_access"},
		"bearer_methods_supported": []string{"header"},
	})
	return b
}

// Challenge is the WWW-Authenticate value for a 401 on /mcp: it points the
// client at the path-scoped PRM so it can discover the authorization server.
func (o *OAuth) Challenge() string {
	meta := o.resource
	if u, err := url.Parse(o.resource); err == nil {
		meta = u.Scheme + "://" + u.Host + WellKnownPRM + u.Path
	}
	return `Bearer resource_metadata="` + meta + `"`
}

// ServePRM serves the metadata document. Public and static by design — it is
// what an unauthenticated client reads to find out how to authenticate.
func (o *OAuth) ServePRM(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(o.prmJSON())
}

// ResourcePath is the path component of the resource URL, used to mount the
// path-scoped PRM alias.
func (o *OAuth) ResourcePath() string {
	if u, err := url.Parse(o.resource); err == nil && u.Path != "" {
		return u.Path
	}
	return "/mcp"
}
