// Package hsdp implements logging in through OpenID Connect providers.
// HSDP IAM is almost but not quite compatible with OIDC standards, hence this connector.
package hsdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/dip-software/go-dip-api/iam"
	"golang.org/x/oauth2"

	"github.com/dexidp/dex/connector"
)

// Config holds configuration options for OpenID Connect logins.
type Config struct {
	Issuer           string    `json:"issuer"`
	InsecureIssuer   string    `json:"insecureIssuer"`
	ClientID         string    `json:"clientID"`
	ClientSecret     string    `json:"clientSecret"`
	RedirectURI      string    `json:"redirectURI"`
	TenantMap        TenantMap `json:"tenantMap"`
	SAML2LoginURL    string    `json:"saml2LoginURL"`
	IAMURL           string    `json:"iamURL"`
	IDMURL           string    `json:"idmURL"`
	EnableGroupClaim bool      `json:"enableGroupClaim"`
	EnableRoleClaim  bool      `json:"enableRoleClaim"`
	RoleAsGroupClaim bool      `json:"roleAsGroupClaim"`

	// Extensions implemented by HSP IAM
	Extension

	// Causes client_secret to be passed as POST parameters instead of basic
	// auth. This is specifically "NOT RECOMMENDED" by the OAuth2 RFC, but some
	// providers require it.
	//
	// https://tools.ietf.org/html/rfc6749#section-2.3.1
	BasicAuthUnsupported *bool `json:"basicAuthUnsupported"`

	Scopes []string `json:"scopes"` // defaults to "profile" and "email"

	// Optional list of whitelisted domains when using Google
	// If this field is nonempty, only users from a listed domain will be allowed to log in
	HostedDomains []string `json:"hostedDomains"`

	// Override the value of email_verified to true in the returned claims
	InsecureSkipEmailVerified bool `json:"insecureSkipEmailVerified"`

	// InsecureEnableGroups enables groups claims. This is disabled by default until https://github.com/dexidp/dex/issues/1065 is resolved
	InsecureEnableGroups bool `json:"insecureEnableGroups"`

	// PromptType will be used fot the prompt parameter (when offline_access, by default prompt=consent)
	PromptType string `json:"promptType"`
}

type Extension struct {
	IntrospectionEndpoint string `json:"introspection_endpoint"`
}

type AudienceTrustMap map[string]string

type TenantMap map[string]string

// ConnectorData stores information for sessions authenticated by this connector
type ConnectorData struct {
	RefreshToken     []byte
	AccessToken      []byte
	Assertion        []byte
	Groups           []string
	TrustedIDPOrg    string
	AudienceTrustMap AudienceTrustMap
	TenantMap        TenantMap
	Introspect       iam.IntrospectResponse
	User             iam.Profile
}

type caller uint

const (
	createCaller caller = iota
	refreshCaller
	exchangeCaller
)

// Open returns a connector which can be used to log in users through an upstream
// OpenID Connect provider.
func (c *Config) Open(id string, logger *slog.Logger) (conn connector.Connector, err error) {
	parentContext, cancel := context.WithCancel(context.Background())

	ctx := oidc.InsecureIssuerURLContext(parentContext, c.InsecureIssuer)

	provider, err := oidc.NewProvider(ctx, c.Issuer)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to get provider: %v", err)
	}

	endpoint := provider.Endpoint()

	// HSP IAM extension
	if err := provider.Claims(&c.Extension); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to get introspection endpoint: %v", err)
	}

	if c.BasicAuthUnsupported != nil {
		// Setting "basicAuthUnsupported" always overrides our detection.
		if *c.BasicAuthUnsupported {
			endpoint.AuthStyle = oauth2.AuthStyleInParams
		}
	}

	scopes := []string{oidc.ScopeOpenID}
	if len(c.Scopes) > 0 {
		filtered := removeElement(c.Scopes, "federated:id") // HSP IAM does not support scopes with colon
		scopes = append(scopes, filtered...)
	} else {
		scopes = append(scopes, "profile", "email", "groups")
	}

	// PromptType should be "consent" by default, if not set
	if c.PromptType == "" {
		c.PromptType = "consent"
	}

	client, err := iam.NewClient(nil, &iam.Config{
		OAuth2ClientID: c.ClientID,
		OAuth2Secret:   c.ClientSecret,
		IAMURL:         c.IAMURL,
		IDMURL:         c.IDMURL,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("error creating HSP IAM client: %w", err)
	}

	clientID := c.ClientID
	return &HSDPConnector{
		provider:      provider,
		client:        client,
		redirectURI:   c.RedirectURI,
		introspectURI: c.IntrospectionEndpoint,
		tenantMap:     c.TenantMap,
		samlLoginURL:  c.SAML2LoginURL,
		clientID:      c.ClientID,
		clientSecret:  c.ClientSecret,
		oauth2Config: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: c.ClientSecret,
			Endpoint:     endpoint,
			Scopes:       scopes,
			RedirectURL:  c.RedirectURI,
		},
		verifier: provider.Verifier(
			&oidc.Config{
				ClientID:        clientID,
				SkipIssuerCheck: true, // Horribly broken currently
			},
		),
		logger:                    logger,
		cancel:                    cancel,
		hostedDomains:             c.HostedDomains,
		insecureSkipEmailVerified: c.InsecureSkipEmailVerified,
		promptType:                c.PromptType,
		enableGroupClaim:          c.EnableGroupClaim,
		enableRoleClaim:           c.EnableRoleClaim,
		roleAsGroupClaim:          c.RoleAsGroupClaim,
	}, nil
}

var (
	_ connector.CallbackConnector = (*HSDPConnector)(nil)
	_ connector.RefreshConnector  = (*HSDPConnector)(nil)
)

type tokenResponse struct {
	Scope        string `json:"scope"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
	IDToken      string `json:"id_token"`
}

type HSDPConnector struct {
	provider                  *oidc.Provider
	client                    *iam.Client
	redirectURI               string
	introspectURI             string
	samlLoginURL              string
	clientID                  string
	clientSecret              string
	oauth2Config              *oauth2.Config
	verifier                  *oidc.IDTokenVerifier
	cancel                    context.CancelFunc
	logger                    *slog.Logger
	hostedDomains             []string
	insecureSkipEmailVerified bool
	enableGroupClaim          bool
	enableRoleClaim           bool
	roleAsGroupClaim          bool
	promptType                string
	tenantMap                 TenantMap
}

func (c *HSDPConnector) isSAML() bool {
	return len(c.samlLoginURL) > 0
}

func (c *HSDPConnector) Close() error {
	c.cancel()
	return nil
}

func (c *HSDPConnector) LoginURL(s connector.Scopes, callbackURL, state string) (string, error) {
	if c.redirectURI != callbackURL {
		return "", fmt.Errorf("expected callback URL %q did not match the URL in the config %q", callbackURL, c.redirectURI)
	}

	// SAML2 flow
	if c.isSAML() {
		cbu, _ := url.Parse(callbackURL)
		values := cbu.Query()
		values.Set("state", state)
		cbu.RawQuery = values.Encode()

		u, err := url.Parse(c.samlLoginURL)
		if err != nil {
			return "", fmt.Errorf("invalid SAML2 login URL: %w", err)
		}
		values = u.Query()
		values.Set("redirect_uri", cbu.String())
		u.RawQuery = values.Encode()
		return u.String(), nil
	}

	var opts []oauth2.AuthCodeOption
	if len(c.hostedDomains) > 0 {
		preferredDomain := c.hostedDomains[0]
		if len(c.hostedDomains) > 1 {
			preferredDomain = "*"
		}
		opts = append(opts, oauth2.SetAuthURLParam("hd", preferredDomain))
	}

	if s.OfflineAccess {
		opts = append(opts, oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", c.promptType))
	}
	return c.oauth2Config.AuthCodeURL(state, opts...), nil
}

type oauth2Error struct {
	error            string
	errorDescription string
}

func (e *oauth2Error) Error() string {
	if e.errorDescription == "" {
		return e.error
	}
	return e.error + ": " + e.errorDescription
}

func (c *HSDPConnector) HandleCallback(s connector.Scopes, r *http.Request) (identity connector.Identity, err error) {
	q := r.URL.Query()
	if errType := q.Get("error"); errType != "" {
		return identity, &oauth2Error{errType, q.Get("error_description")}
	}

	// SAML2 flow
	if c.isSAML() {
		assertion := q.Get("assertion")
		form := url.Values{}
		form.Add("grant_type", "urn:ietf:params:oauth:grant-type:saml2-bearer")
		form.Add("assertion", assertion)
		requestBody := form.Encode()
		req, _ := http.NewRequest(http.MethodPost, c.oauth2Config.Endpoint.TokenURL, io.NopCloser(strings.NewReader(requestBody)))
		req.SetBasicAuth(c.clientID, c.clientSecret)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Api-Version", "2")
		req.ContentLength = int64(len(requestBody))

		resp, err := doRequest(r.Context(), req)
		if err != nil {
			return identity, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return identity, err
		}
		if resp.StatusCode != http.StatusOK {
			return identity, fmt.Errorf("%s: %s", resp.Status, body)
		}

		var tr tokenResponse
		if err := json.Unmarshal(body, &tr); err != nil {
			return identity, fmt.Errorf("hsdp: failed to token response: %v", err)
		}
		token := &oauth2.Token{
			AccessToken:  tr.AccessToken,
			TokenType:    tr.TokenType,
			RefreshToken: tr.RefreshToken,
			Expiry:       time.Unix(tr.ExpiresIn, 0),
		}
		return c.createIdentity(r.Context(), identity, token, r, createCaller)
	}

	token, err := c.oauth2Config.Exchange(r.Context(), q.Get("code"))
	if err != nil {
		return identity, fmt.Errorf("oidc: failed to get token: %v", err)
	}

	return c.createIdentity(r.Context(), identity, token, r, createCaller)
}

// Refresh is used to refresh a session with the refresh token provided by the IdP
func (c *HSDPConnector) Refresh(ctx context.Context, s connector.Scopes, identity connector.Identity) (connector.Identity, error) {
	cd := ConnectorData{}
	err := json.Unmarshal(identity.ConnectorData, &cd)
	if err != nil {
		return identity, fmt.Errorf("oidc: failed to unmarshal connector data: %v", err)
	}

	t := &oauth2.Token{
		RefreshToken: string(cd.RefreshToken),
		Expiry:       time.Now().Add(-time.Hour),
	}
	token, err := c.oauth2Config.TokenSource(ctx, t).Token()
	if err != nil {
		return identity, fmt.Errorf("oidc: failed to get refresh token: %v", err)
	}

	return c.createIdentity(ctx, identity, token, nil, refreshCaller)
}

func (c *HSDPConnector) TokenIdentity(ctx context.Context, subjectTokenType, subjectToken string) (connector.Identity, error) {
	var identity connector.Identity
	token := &oauth2.Token{
		AccessToken: subjectToken,
		TokenType:   "Bearer",
	}
	return c.createIdentity(ctx, identity, token, nil, exchangeCaller)
}

func (c *HSDPConnector) createIdentity(ctx context.Context, identity connector.Identity, token *oauth2.Token, r *http.Request, caller caller) (connector.Identity, error) {
	var claims map[string]interface{}

	cd := ConnectorData{}

	if caller == createCaller && c.isSAML() && r != nil {
		// Save assertion
		q := r.URL.Query()
		assertion := q.Get("assertion")
		cd.Assertion = []byte(assertion)
	}

	// We immediately want to run getUserInfo if configured before we validate the claims
	userInfo, err := c.provider.UserInfo(ctx, oauth2.StaticTokenSource(token))
	if err != nil {
		return identity, fmt.Errorf("hsdp: error loading userinfo: %v", err)
	}
	if err := userInfo.Claims(&claims); err != nil {
		return identity, fmt.Errorf("hsdp: failed to decode userinfo claims: %v", err)
	}
	// Introspect so we can get group assignments
	introspectResponse, err := c.introspect(ctx, oauth2.StaticTokenSource(token))
	if err != nil {
		return identity, fmt.Errorf("hsdp: introspect failed: %w", err)
	}

	hasEmailScope := false
	for _, s := range c.oauth2Config.Scopes {
		if s == "email" {
			hasEmailScope = true
			break
		}
	}

	email, found := claims["email"].(string)
	// For Service identities we take sub as email claim
	if introspectResponse.IdentityType == "Service" {
		email = introspectResponse.Sub
		found = true
	}
	if !found && hasEmailScope {
		return identity, errors.New("missing \"email\" claim")
	}

	emailVerified := true

	if c.isSAML() { // For SAML2 we claim email verification for now
		emailVerified = true
	}
	hostedDomain, _ := claims["hd"].(string)

	if len(c.hostedDomains) > 0 {
		found := false
		for _, domain := range c.hostedDomains {
			if hostedDomain == domain {
				found = true
				break
			}
		}
		if !found {
			return identity, fmt.Errorf("hsdp: unexpected hd claim %v", hostedDomain)
		}
	}

	cd.RefreshToken = []byte(token.RefreshToken)
	cd.AccessToken = []byte(token.AccessToken)
	cd.Introspect = *introspectResponse

	// Get user info for profile details
	user, _, err := c.client.WithToken(token.AccessToken).Users.LegacyGetUserByUUID(introspectResponse.Sub)
	if err != nil {
		c.logger.Error("failed to get user profile", "error", err)
	}
	if user != nil {
		cd.User = *user
	}

	identity = connector.Identity{
		UserID:        introspectResponse.Sub,
		Username:      introspectResponse.Username,
		Email:         email,
		EmailVerified: emailVerified,
	}

	// Attach connector data
	connData, err := json.Marshal(&cd)
	if err != nil {
		return identity, fmt.Errorf("oidc: failed to encode connector data: %v", err)
	}
	identity.ConnectorData = connData

	return identity, nil
}

// removeElement removes an element from a slice. It works for any ordered type (e.g., numbers, strings).
func removeElement[T comparable](slice []T, elementToRemove T) []T {
	var newSlice []T
	for _, item := range slice {
		if item != elementToRemove {
			newSlice = append(newSlice, item)
		}
	}
	return newSlice
}
