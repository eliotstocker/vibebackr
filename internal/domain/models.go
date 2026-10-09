package domain

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrInvalidSlug indicates a project slug does not meet format or reserved-word rules.
var ErrInvalidSlug = errors.New("invalid slug")

var reservedSlugs = map[string]bool{
	"www":          true,
	"api":          true,
	"mcp":          true,
	"auth":         true,
	"oauth":        true,
	"login":        true,
	"guide":        true,
	"client-guide": true,
	"nerds":        true,
	"relay":        true,
	"llms":         true,
	"apps":         true,
	"health":       true,
	"admin":        true,
	"mail":         true,
	"static":       true,
	"assets":       true,
}

// ValidateSlug normalizes a project slug to lowercase and validates that it contains
// only alphanumeric characters and internal dashes (1-63 chars) and is not a reserved name.
// An empty slug is valid (representing no custom slug).
func ValidateSlug(raw string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return "", nil
	}
	if len(s) > 63 {
		return "", fmt.Errorf("%w: slug cannot exceed 63 characters", ErrInvalidSlug)
	}
	if s[0] == '-' || s[len(s)-1] == '-' {
		return "", fmt.Errorf("%w: slug cannot start or end with a dash", ErrInvalidSlug)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return "", fmt.Errorf("%w: slug must contain only alphanumeric characters and dashes", ErrInvalidSlug)
		}
	}
	if reservedSlugs[s] {
		return "", fmt.Errorf("%w: %q is a reserved name", ErrInvalidSlug, s)
	}
	return s, nil
}

// User roles in VibeBackr.
const (
	RoleAdmin   = "admin"
	RoleManager = "manager"
	RoleMember  = "member"
)

// Collection visibilities.
const (
	VisibilityPrivate = "private"
	VisibilityPublic  = "public"
	VisibilityShared  = "shared"
)

// User represents an identity in the system.
// Hierarchy is modeled as an adjacency list via ManagerID.
type User struct {
	ID           string    `json:"id" db:"id"`
	TenantID     *string   `json:"tenant_id,omitempty" db:"tenant_id"`
	Email        string    `json:"email" db:"email"`
	Name         string    `json:"name" db:"name"`
	Role         string    `json:"role" db:"role"`
	ManagerID    *string   `json:"manager_id,omitempty" db:"manager_id"`
	TokenVersion string    `json:"token_version" db:"token_version"`
	CreatedAt    time.Time `json:"created_at" db:"created_at"`
}

// Invite pre-authorizes user onboarding and manager hierarchy assignment.
type Invite struct {
	ID        string    `json:"id" db:"id"`
	TenantID  string    `json:"tenant_id,omitempty" db:"tenant_id"`
	Email     string    `json:"email" db:"email"`
	Role      string    `json:"role" db:"role"`
	ManagerID *string   `json:"manager_id,omitempty" db:"manager_id"`
	ExpiresAt time.Time `json:"expires_at" db:"expires_at"`
	CreatedBy string    `json:"created_by" db:"created_by"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
}

// TenantMembership links a user to a tenant with a specific role and optional manager.
type TenantMembership struct {
	UserID    string    `json:"user_id" db:"user_id"`
	TenantID  string    `json:"tenant_id" db:"tenant_id"`
	Role      string    `json:"role" db:"role"`
	ManagerID *string   `json:"manager_id,omitempty" db:"manager_id"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
}

// JSONStrings represents a slice of strings stored as JSON in text columns.
type JSONStrings []string

// Value implements driver.Valuer.
func (j JSONStrings) Value() (driver.Value, error) {
	if j == nil {
		return "[]", nil
	}
	b, err := json.Marshal(j)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// Scan implements sql.Scanner.
func (j *JSONStrings) Scan(src interface{}) error {
	if src == nil {
		*j = []string{}
		return nil
	}
	var source []byte
	switch v := src.(type) {
	case []byte:
		source = v
	case string:
		source = []byte(v)
	default:
		return errors.New("incompatible type for JSONStrings")
	}
	if len(source) == 0 {
		*j = []string{}
		return nil
	}
	return json.Unmarshal(source, j)
}

// Collection groups records and defines namespace-level visibility.
type Collection struct {
	ID                  string      `json:"id" db:"id"`
	TenantID            string      `json:"tenant_id" db:"tenant_id"`
	Name                string      `json:"name" db:"name"`
	Slug                string      `json:"slug,omitempty" db:"slug"`
	DefaultVisibility   string      `json:"default_visibility" db:"default_visibility"`
	AllowedDomains      JSONStrings `json:"allowed_domains" db:"allowed_domains"`
	OriginURLs          JSONStrings `json:"origin_urls" db:"origin_urls"`
	VectorSearchEnabled bool        `json:"vector_search_enabled" db:"vector_search_enabled"`
	AutoInviteShares    bool        `json:"auto_invite_shares" db:"auto_invite_shares"`
	CreatedAt           time.Time   `json:"created_at" db:"created_at"`
}

// AccessPolicy defines visibility and explicit email access control list.
type AccessPolicy struct {
	Visibility string   `json:"visibility"`       // "public", "private", "shared"
	Shares     []string `json:"shares,omitempty"` // List of user emails with read/write access
}

// Value implements driver.Valuer.
func (a AccessPolicy) Value() (driver.Value, error) {
	b, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// Scan implements sql.Scanner.
func (a *AccessPolicy) Scan(src interface{}) error {
	if src == nil {
		*a = AccessPolicy{Visibility: VisibilityPrivate}
		return nil
	}
	var source []byte
	switch v := src.(type) {
	case []byte:
		source = v
	case string:
		source = []byte(v)
	default:
		return errors.New("incompatible type for AccessPolicy")
	}
	if len(source) == 0 {
		*a = AccessPolicy{Visibility: VisibilityPrivate}
		return nil
	}
	return json.Unmarshal(source, a)
}

// Record is the universal document structure across all collections.
type Record struct {
	ID           string       `json:"id" db:"id"`
	CollectionID string       `json:"collection_id" db:"collection_id"`
	Title        string       `json:"title" db:"title"`
	Content      string       `json:"content" db:"content"`
	Data         string       `json:"data" db:"data"` // JSON custom payload
	CreatedBy    string       `json:"created_by" db:"created_by"`
	AccessPolicy AccessPolicy `json:"access_policy" db:"access_policy"`
	CreatedAt    time.Time    `json:"created_at" db:"created_at"`
	UpdatedAt    time.Time    `json:"updated_at" db:"updated_at"`
}

// RecordEmbedding holds 768-dimensional vector chunks for RAG.
type RecordEmbedding struct {
	ID           string    `json:"id" db:"id"`
	RecordID     string    `json:"record_id" db:"record_id"`
	ContentChunk string    `json:"content_chunk" db:"content_chunk"`
	Embedding    []float32 `json:"embedding,omitempty" db:"embedding"`
	CreatedAt    time.Time `json:"created_at" db:"created_at"`
}

// OAuthClient stores dynamically registered clients (RFC 7591).
type OAuthClient struct {
	ID           string      `json:"client_id" db:"id"`
	ClientSecret string      `json:"client_secret,omitempty" db:"client_secret"`
	ClientName   string      `json:"client_name" db:"client_name"`
	RedirectURIs JSONStrings `json:"redirect_uris" db:"redirect_uris"`
	GrantTypes   JSONStrings `json:"grant_types" db:"grant_types"`
	CreatedAt    time.Time   `json:"created_at" db:"created_at"`
}

// AuthCode holds temporary PKCE authorization codes.
type AuthCode struct {
	Code                string    `db:"code"`
	ClientID            string    `db:"client_id"`
	UserID              string    `db:"user_id"`
	CodeChallenge       string    `db:"code_challenge"`
	CodeChallengeMethod string    `db:"code_challenge_method"`
	ExpiresAt           time.Time `db:"expires_at"`
}

// RefreshToken represents a long-lived OAuth/session refresh token.
type RefreshToken struct {
	ID           string     `json:"id" db:"id"`
	UserID       string     `json:"user_id" db:"user_id"`
	ClientID     string     `json:"client_id" db:"client_id"`
	TokenHash    string     `json:"-" db:"token_hash"`
	TokenVersion string     `json:"token_version" db:"token_version"`
	ExpiresAt    time.Time  `json:"expires_at" db:"expires_at"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty" db:"revoked_at"`
	CreatedAt    time.Time  `json:"created_at" db:"created_at"`
}
