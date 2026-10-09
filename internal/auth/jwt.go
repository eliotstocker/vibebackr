package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/eliotstocker/vibebackr/internal/domain"
	"github.com/golang-jwt/jwt/v5"
)

// Claims represents the JWT payload carrying sub, role, and manager_id.
type Claims struct {
	UserID       string  `json:"sub"`
	Email        string  `json:"email"`
	Role         string  `json:"role"`
	ManagerID    *string `json:"manager_id,omitempty"`
	TokenVersion string  `json:"token_version,omitempty"`
	jwt.RegisteredClaims
}

// TokenManager handles issuing and validating JWTs.
type TokenManager struct {
	secret     []byte
	ttl        time.Duration // Access token lifetime (default 15m)
	refreshTTL time.Duration // Refresh token lifetime (default 30d)
}

// NewTokenManager creates a new JWT manager.
func NewTokenManager(secret string, ttl time.Duration) *TokenManager {
	if ttl <= 0 {
		ttl = 15 * time.Minute // Short-lived 15-minute access token
	}
	return &TokenManager{
		secret:     []byte(secret),
		ttl:        ttl,
		refreshTTL: 30 * 24 * time.Hour, // 30-day refresh token
	}
}

// TTL returns the configured access token lifetime.
func (tm *TokenManager) TTL() time.Duration {
	if tm == nil || tm.ttl <= 0 {
		return 15 * time.Minute
	}
	return tm.ttl
}

// RefreshTTL returns the configured refresh token lifetime.
func (tm *TokenManager) RefreshTTL() time.Duration {
	if tm == nil || tm.refreshTTL <= 0 {
		return 30 * 24 * time.Hour
	}
	return tm.refreshTTL
}

// SetRefreshTTL sets the refresh token lifetime.
func (tm *TokenManager) SetRefreshTTL(ttl time.Duration) {
	if ttl > 0 {
		tm.refreshTTL = ttl
	}
}

// GenerateRefreshToken generates a cryptographically secure random refresh token string and its SHA-256 hash.
func GenerateRefreshToken() (rawToken string, tokenHash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	rawToken = "rt_" + hex.EncodeToString(b)
	tokenHash = HashRefreshToken(rawToken)
	return rawToken, tokenHash, nil
}

// HashRefreshToken returns the SHA-256 hex hash of a raw refresh token string.
func HashRefreshToken(rawToken string) string {
	hash := sha256.Sum256([]byte(rawToken))
	return hex.EncodeToString(hash[:])
}

// GenerateToken generates a signed JWT token for a user.
func (tm *TokenManager) GenerateToken(u *domain.User) (string, error) {
	now := time.Now().UTC()
	claims := Claims{
		UserID:       u.ID,
		Email:        u.Email,
		Role:         u.Role,
		ManagerID:    u.ManagerID,
		TokenVersion: u.TokenVersion,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   u.ID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(tm.ttl)),
			Issuer:    "vibebackr",
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(tm.secret)
}

// ValidateToken parses and verifies a Bearer token string.
func (tm *TokenManager) ValidateToken(tokenStr string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return tm.secret, nil
	}, jwt.WithExpirationRequired(), jwt.WithIssuer("vibebackr"))
	if err != nil {
		return nil, err
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, errors.New("invalid token")
	}

	return claims, nil
}
