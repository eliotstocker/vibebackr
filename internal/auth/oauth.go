package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"

	"github.com/eliotstocker/vibebackr/internal/domain"
	"github.com/google/uuid"
)

// DCRRegistrationRequest represents RFC 7591 client metadata.
type DCRRegistrationRequest struct {
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

// GenerateRandomString generates a secure crypto-random hex string.
func GenerateRandomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// VerifyPKCE validates code_verifier against code_challenge using S256 or plain.
func VerifyPKCE(verifier, challenge, method string) bool {
	if verifier == "" || challenge == "" {
		return false
	}
	switch strings.ToUpper(method) {
	case "S256":
		h := sha256.Sum256([]byte(verifier))
		computed := base64.RawURLEncoding.EncodeToString(h[:])
		return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
	case "PLAIN", "":
		return subtle.ConstantTimeCompare([]byte(verifier), []byte(challenge)) == 1
	default:
		return false
	}
}

// RegisterClient creates a new OAuth client record per RFC 7591.
func RegisterClient(req DCRRegistrationRequest) *domain.OAuthClient {
	grantTypes := req.GrantTypes
	if len(grantTypes) == 0 {
		grantTypes = []string{"authorization_code", "refresh_token", "client_credentials"}
	}
	name := req.ClientName
	if name == "" {
		name = "Autonomous Agent " + GenerateRandomString(4)
	}

	return &domain.OAuthClient{
		ID:           "client_" + uuid.NewString(),
		ClientSecret: "sec_" + GenerateRandomString(24),
		ClientName:   name,
		RedirectURIs: req.RedirectURIs,
		GrantTypes:   grantTypes,
	}
}

// ValidateRedirectURI checks if the requested redirect URI is permitted for the client.
// Per RFC 8252 (OAuth 2.0 for Native Apps), dynamically assigned loopback ports on localhost/127.0.0.1 are allowed.
func ValidateRedirectURI(client *domain.OAuthClient, uri string) error {
	if uri == "" {
		return errors.New("missing redirect_uri")
	}
	reqU, reqErr := url.Parse(uri)
	if reqErr != nil || reqU.Scheme == "" {
		return errors.New("invalid redirect_uri")
	}
	scheme := strings.ToLower(reqU.Scheme)
	if scheme == "javascript" || scheme == "data" || scheme == "vbscript" {
		return errors.New("forbidden redirect_uri scheme")
	}

	if len(client.RedirectURIs) == 0 {
		host := strings.ToLower(reqU.Hostname())
		if scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1") {
			return nil
		}
		return errors.New("unauthorized redirect_uri: client has no registered redirect_uris")
	}

	for _, allowed := range client.RedirectURIs {
		if allowed == uri {
			return nil
		}
		// RFC 8252 loopback port flexibility for native clients (e.g. Claude Desktop)
		if allowU, aErr := url.Parse(allowed); aErr == nil {
			if allowU.Scheme == "http" && reqU.Scheme == "http" &&
				allowU.Path == reqU.Path &&
				(allowU.Hostname() == "localhost" || allowU.Hostname() == "127.0.0.1") &&
				(reqU.Hostname() == "localhost" || reqU.Hostname() == "127.0.0.1") {
				return nil
			}
		}
	}
	return errors.New("unauthorized redirect_uri")
}
