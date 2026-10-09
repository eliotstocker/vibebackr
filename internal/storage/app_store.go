package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// MaxAppHTMLBytes caps single-file HTML uploads at 512 KB.
const MaxAppHTMLBytes = 512 * 1024

// AppStore persists and serves single-file HTML applications per collection slug.
type AppStore interface {
	PutApp(ctx context.Context, slug string, htmlContent string) error
	GetApp(ctx context.Context, slug string) (string, error)
	DeleteApp(ctx context.Context, slug string) error
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// CollectionSlug normalizes a collection ID or custom project slug for subdomain/path routing.
// Standard 36-char UUIDs are converted to 32-char dashless hex; custom project slugs preserve dashes.
func CollectionSlug(slugOrID string) string {
	trimmed := strings.ToLower(strings.TrimSpace(slugOrID))
	if isUUID(trimmed) {
		return strings.ReplaceAll(trimmed, "-", "")
	}
	var b strings.Builder
	b.Grow(len(trimmed))
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// EffectiveSlug returns the custom project slug if set, otherwise the dashless collection ID.
func EffectiveSlug(collectionID, customSlug string) string {
	if s := CollectionSlug(customSlug); s != "" {
		return s
	}
	return CollectionSlug(collectionID)
}

// AppURL resolves the public URL for a hosted collection app given the canonical baseURL and slug or collectionID.
func AppURL(baseURL, slugOrID string) string {
	slug := CollectionSlug(slugOrID)
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || parsed.Host == "" {
		return fmt.Sprintf("https://%s.vibebac.kr", slug)
	}
	host := parsed.Hostname()
	if host == "localhost" || host == "127.0.0.1" || strings.HasPrefix(host, "192.168.") || strings.HasPrefix(host, "10.") {
		return fmt.Sprintf("%s/apps/%s", strings.TrimRight(baseURL, "/"), slug)
	}
	scheme := parsed.Scheme
	if scheme == "" {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s.%s", scheme, slug, parsed.Host)
}

// ValidateAppHTML checks that the HTML payload is non-empty and within the size limit.
func ValidateAppHTML(htmlContent string) error {
	trimmed := strings.TrimSpace(htmlContent)
	if trimmed == "" {
		return errors.New("html content cannot be empty")
	}
	if len(htmlContent) > MaxAppHTMLBytes {
		return fmt.Errorf("html content exceeds maximum size of %d bytes", MaxAppHTMLBytes)
	}
	return nil
}

// NewAppStoreFromEnv returns an S3AppStore if S3_BUCKET is configured, otherwise a LocalAppStore.
func NewAppStoreFromEnv() AppStore {
	bucket := os.Getenv("S3_BUCKET")
	region := os.Getenv("S3_REGION")
	if region == "" {
		region = os.Getenv("AWS_REGION")
	}
	if region == "" {
		region = os.Getenv("AWS_DEFAULT_REGION")
	}
	if region == "" {
		region = "eu-west-2"
	}
	if bucket != "" {
		return NewS3AppStore(bucket, region)
	}
	return NewLocalAppStore("")
}

// LocalAppStore stores HTML apps in memory and optionally on disk for local development.
type LocalAppStore struct {
	baseDir string
	mu      sync.RWMutex
	mem     map[string]string
}

// NewLocalAppStore initializes a local app store.
func NewLocalAppStore(baseDir string) *LocalAppStore {
	if baseDir == "" {
		for _, candidate := range []string{"web/dist", "../../web/dist"} {
			if st, err := os.Stat(candidate); err == nil && st.IsDir() {
				baseDir = filepath.Join(candidate, "apps")
				break
			}
		}
	}
	return &LocalAppStore{
		baseDir: baseDir,
		mem:     make(map[string]string),
	}
}

func (l *LocalAppStore) PutApp(_ context.Context, slug string, htmlContent string) error {
	if err := ValidateAppHTML(htmlContent); err != nil {
		return err
	}
	slug = CollectionSlug(slug)
	l.mu.Lock()
	l.mem[slug] = htmlContent
	l.mu.Unlock()

	if l.baseDir != "" {
		dir := filepath.Join(l.baseDir, slug)
		if err := os.MkdirAll(dir, 0755); err == nil {
			_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte(htmlContent), 0644)
		}
	}
	return nil
}

func (l *LocalAppStore) GetApp(_ context.Context, slug string) (string, error) {
	slug = CollectionSlug(slug)
	l.mu.RLock()
	val, ok := l.mem[slug]
	l.mu.RUnlock()
	if ok {
		return val, nil
	}
	if l.baseDir != "" {
		b, err := os.ReadFile(filepath.Join(l.baseDir, slug, "index.html"))
		if err == nil {
			return string(b), nil
		}
	}
	return "", os.ErrNotExist
}

func (l *LocalAppStore) DeleteApp(_ context.Context, slug string) error {
	slug = CollectionSlug(slug)
	l.mu.Lock()
	delete(l.mem, slug)
	l.mu.Unlock()
	if l.baseDir != "" {
		_ = os.RemoveAll(filepath.Join(l.baseDir, slug))
	}
	return nil
}

// S3AppStore writes single-file HTML apps directly to S3 using pure-Go AWS SigV4.
type S3AppStore struct {
	Bucket     string
	Region     string
	Endpoint   string // Optional custom endpoint for testing
	HTTPClient *http.Client
	 fallback  *LocalAppStore
}

// NewS3AppStore creates an S3-backed AppStore.
func NewS3AppStore(bucket, region string) *S3AppStore {
	return &S3AppStore{
		Bucket:     bucket,
		Region:     region,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
		fallback:   NewLocalAppStore(""),
	}
}

func (s *S3AppStore) objectURL(slug string) (string, string) {
	slug = CollectionSlug(slug)
	key := fmt.Sprintf("/apps/%s/index.html", slug)
	if s.Endpoint != "" {
		ep := strings.TrimRight(s.Endpoint, "/")
		u, err := url.Parse(ep)
		if err == nil {
			return ep + key, u.Host
		}
	}
	host := fmt.Sprintf("%s.s3.%s.amazonaws.com", s.Bucket, s.Region)
	return fmt.Sprintf("https://%s%s", host, key), host
}

func (s *S3AppStore) PutApp(ctx context.Context, slug string, htmlContent string) error {
	if err := ValidateAppHTML(htmlContent); err != nil {
		return err
	}
	_ = s.fallback.PutApp(ctx, slug, htmlContent)

	accessKey := os.Getenv("AWS_ACCESS_KEY_ID")
	secretKey := os.Getenv("AWS_SECRET_ACCESS_KEY")
	sessionToken := os.Getenv("AWS_SESSION_TOKEN")
	if accessKey == "" || secretKey == "" {
		return nil
	}

	targetURL, host := s.objectURL(slug)
	bodyBytes := []byte(htmlContent)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}

	// Aggressive edge caching with ETag revalidation:
	// - Browser always validates (max-age=0, must-revalidate)
	// - CloudFront serves pure edge cache for 5s and revalidates via cheap S3 ETag 304 checks afterward,
	//   eliminating any need for CloudFront CreateInvalidation calls.
	req.Header.Set("Content-Type", "text/html; charset=utf-8")
	req.Header.Set("Cache-Control", "public, max-age=0, s-maxage=5, must-revalidate")
	req.Host = host

	signS3Request(req, bodyBytes, accessKey, secretKey, sessionToken, s.Region, time.Now().UTC())

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("s3 put failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("s3 put returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

func (s *S3AppStore) GetApp(ctx context.Context, slug string) (string, error) {
	return s.fallback.GetApp(ctx, slug)
}

func (s *S3AppStore) DeleteApp(ctx context.Context, slug string) error {
	_ = s.fallback.DeleteApp(ctx, slug)

	accessKey := os.Getenv("AWS_ACCESS_KEY_ID")
	secretKey := os.Getenv("AWS_SECRET_ACCESS_KEY")
	sessionToken := os.Getenv("AWS_SESSION_TOKEN")
	if accessKey == "" || secretKey == "" {
		return nil
	}

	targetURL, host := s.objectURL(slug)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, targetURL, nil)
	if err != nil {
		return err
	}
	req.Host = host

	signS3Request(req, nil, accessKey, secretKey, sessionToken, s.Region, time.Now().UTC())

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("s3 delete failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("s3 delete returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

func signS3Request(req *http.Request, payload []byte, accessKey, secretKey, sessionToken, region string, now time.Time) {
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	payloadHash := sha256Hex(payload)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	if sessionToken != "" {
		req.Header.Set("x-amz-security-token", sessionToken)
	}

	headersToSign := map[string]string{
		"host":                 req.Host,
		"x-amz-content-sha256": payloadHash,
		"x-amz-date":           amzDate,
	}
	if ct := req.Header.Get("Content-Type"); ct != "" {
		headersToSign["content-type"] = ct
	}
	if cc := req.Header.Get("Cache-Control"); cc != "" {
		headersToSign["cache-control"] = cc
	}
	if sessionToken != "" {
		headersToSign["x-amz-security-token"] = sessionToken
	}

	var signedHeaderNames []string
	for k := range headersToSign {
		signedHeaderNames = append(signedHeaderNames, k)
	}
	sort.Strings(signedHeaderNames)

	var canonicalHeaders strings.Builder
	for _, k := range signedHeaderNames {
		canonicalHeaders.WriteString(k)
		canonicalHeaders.WriteByte(':')
		canonicalHeaders.WriteString(strings.TrimSpace(headersToSign[k]))
		canonicalHeaders.WriteByte('\n')
	}
	signedHeaders := strings.Join(signedHeaderNames, ";")

	canonicalURI := req.URL.EscapedPath()
	if canonicalURI == "" {
		canonicalURI = "/"
	}

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		"",
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	credentialScope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, region)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+secretKey), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte("s3"))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	signature := hex.EncodeToString(hmacSHA256(kSigning, []byte(stringToSign)))

	authHeader := fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, credentialScope, signedHeaders, signature,
	)
	req.Header.Set("Authorization", authHeader)
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}
