package billing

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	goCardlessSandboxURL = "https://api-sandbox.gocardless.com"
	goCardlessLiveURL    = "https://api.gocardless.com"
	goCardlessAPIVersion = "2015-07-06"
)

// GoCardlessClient is a zero-dependency HTTP client for the GoCardless REST API.
type GoCardlessClient struct {
	accessToken   string
	webhookSecret string
	baseURL       string
	httpClient    *http.Client
}

// BillingRequestInfo summarizes a GoCardless Billing Request.
type BillingRequestInfo struct {
	ID         string
	Status     string
	CustomerID string
	MandateID  string
	TenantID   string
	Plan       string
}

// WebhookEvent represents a single event inside a GoCardless webhook payload.
type WebhookEvent struct {
	ID           string `json:"id"`
	ResourceType string `json:"resource_type"`
	Action       string `json:"action"`
	Links        struct {
		BillingRequest string `json:"billing_request"`
		Mandate        string `json:"mandate"`
		Subscription   string `json:"subscription"`
		Payment        string `json:"payment"`
		Customer       string `json:"customer"`
	} `json:"links"`
}

// NewGoCardlessClient initializes a GoCardless API client.
func NewGoCardlessClient(accessToken, webhookSecret, env, customBaseURL string) *GoCardlessClient {
	baseURL := strings.TrimRight(customBaseURL, "/")
	if baseURL == "" {
		e := strings.ToLower(strings.TrimSpace(env))
		if e == "live" || (e == "" && strings.HasPrefix(accessToken, "live_")) {
			baseURL = goCardlessLiveURL
		} else {
			baseURL = goCardlessSandboxURL
		}
	}
	return &GoCardlessClient{
		accessToken:   strings.TrimSpace(accessToken),
		webhookSecret: strings.TrimSpace(webhookSecret),
		baseURL:       baseURL,
		httpClient:    &http.Client{Timeout: 15 * time.Second},
	}
}

// IsConfigured returns true if a GoCardless access token is set.
func (c *GoCardlessClient) IsConfigured() bool {
	return c != nil && c.accessToken != ""
}

// SetHTTPClient overrides the underlying HTTP client (useful for in-memory test transports).
func (c *GoCardlessClient) SetHTTPClient(hc *http.Client) {
	if c != nil && hc != nil {
		c.httpClient = hc
	}
}

func (c *GoCardlessClient) doRequest(ctx context.Context, method, path string, idempotencyKey string, body interface{}, out interface{}) error {
	if !c.IsConfigured() {
		return errors.New("gocardless access token is not configured")
	}

	var reqReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("failed marshaling gocardless request: %w", err)
		}
		reqReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqReader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.accessToken)
	req.Header.Set("GoCardless-Version", goCardlessAPIVersion)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("gocardless request failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return fmt.Errorf("failed reading gocardless response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errEnv struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(respBytes, &errEnv) == nil && errEnv.Error.Message != "" {
			return fmt.Errorf("gocardless API error (%d): %s", resp.StatusCode, errEnv.Error.Message)
		}
		return fmt.Errorf("gocardless API returned status %d: %s", resp.StatusCode, string(respBytes))
	}

	if out != nil && len(respBytes) > 0 {
		if err := json.Unmarshal(respBytes, out); err != nil {
			return fmt.Errorf("failed decoding gocardless response: %w", err)
		}
	}
	return nil
}

// CreateBillingRequest creates a GBP Direct Debit mandate billing request for the tenant.
func (c *GoCardlessClient) CreateBillingRequest(ctx context.Context, tenantID, plan string) (string, error) {
	payload := map[string]interface{}{
		"billing_requests": map[string]interface{}{
			"mandate_request": map[string]interface{}{
				"scheme":   "bacs",
				"currency": "GBP",
				"metadata": map[string]string{
					"tenant_id": tenantID,
					"plan":      plan,
				},
			},
			"metadata": map[string]string{
				"tenant_id": tenantID,
				"plan":      plan,
			},
		},
	}

	var resp struct {
		BillingRequests struct {
			ID string `json:"id"`
		} `json:"billing_requests"`
	}
	if err := c.doRequest(ctx, http.MethodPost, "/billing_requests", "", payload, &resp); err != nil {
		return "", err
	}
	if resp.BillingRequests.ID == "" {
		return "", errors.New("gocardless returned empty billing_request id")
	}
	return resp.BillingRequests.ID, nil
}

// CreateBillingRequestFlow creates a hosted checkout flow for a billing request and returns its authorization URL.
func (c *GoCardlessClient) CreateBillingRequestFlow(ctx context.Context, billingReqID, redirectURI, exitURI string) (string, error) {
	payload := map[string]interface{}{
		"billing_request_flows": map[string]interface{}{
			"redirect_uri": redirectURI,
			"exit_uri":     exitURI,
			"links": map[string]string{
				"billing_request": billingReqID,
			},
		},
	}

	var resp struct {
		BillingRequestFlows struct {
			AuthorisationURL string `json:"authorisation_url"`
		} `json:"billing_request_flows"`
	}
	if err := c.doRequest(ctx, http.MethodPost, "/billing_request_flows", "", payload, &resp); err != nil {
		return "", err
	}
	if resp.BillingRequestFlows.AuthorisationURL == "" {
		return "", errors.New("gocardless returned empty authorisation_url")
	}
	return resp.BillingRequestFlows.AuthorisationURL, nil
}

// GetBillingRequest retrieves a billing request and extracts linked customer and mandate IDs.
func (c *GoCardlessClient) GetBillingRequest(ctx context.Context, billingReqID string) (*BillingRequestInfo, error) {
	var resp struct {
		BillingRequests struct {
			ID       string            `json:"id"`
			Status   string            `json:"status"`
			Metadata map[string]string `json:"metadata"`
			Links    struct {
				Customer              string `json:"customer"`
				MandateRequestMandate string `json:"mandate_request_mandate"`
			} `json:"links"`
			MandateRequest struct {
				Links struct {
					Mandate string `json:"mandate"`
				} `json:"links"`
			} `json:"mandate_request"`
		} `json:"billing_requests"`
	}
	if err := c.doRequest(ctx, http.MethodGet, "/billing_requests/"+billingReqID, "", nil, &resp); err != nil {
		return nil, err
	}

	br := resp.BillingRequests
	mandateID := br.Links.MandateRequestMandate
	if mandateID == "" {
		mandateID = br.MandateRequest.Links.Mandate
	}

	return &BillingRequestInfo{
		ID:         br.ID,
		Status:     br.Status,
		CustomerID: br.Links.Customer,
		MandateID:  mandateID,
		TenantID:   br.Metadata["tenant_id"],
		Plan:       br.Metadata["plan"],
	}, nil
}

// CreateSubscription creates a recurring monthly GBP subscription on an active mandate.
func (c *GoCardlessClient) CreateSubscription(ctx context.Context, mandateID string, amountPence int, planName, tenantID string) (string, error) {
	idempotencyKey := fmt.Sprintf("sub-%s-%s-%s", tenantID, mandateID, planName)
	payload := map[string]interface{}{
		"subscriptions": map[string]interface{}{
			"amount":        amountPence,
			"currency":      "GBP",
			"name":          fmt.Sprintf("VibeBackr %s Plan", strings.Title(planName)),
			"interval_unit": "monthly",
			"links": map[string]string{
				"mandate": mandateID,
			},
			"metadata": map[string]string{
				"tenant_id": tenantID,
				"plan":      planName,
			},
		},
	}

	var resp struct {
		Subscriptions struct {
			ID string `json:"id"`
		} `json:"subscriptions"`
	}
	if err := c.doRequest(ctx, http.MethodPost, "/subscriptions", idempotencyKey, payload, &resp); err != nil {
		return "", err
	}
	if resp.Subscriptions.ID == "" {
		return "", errors.New("gocardless returned empty subscription id")
	}
	return resp.Subscriptions.ID, nil
}

// ChargeOneOffPayment creates an immediate one-off GBP payment against an existing mandate (e.g. £2 upgrade fee).
func (c *GoCardlessClient) ChargeOneOffPayment(ctx context.Context, mandateID string, amountPence int, description, idempotencyKey string) (string, error) {
	payload := map[string]interface{}{
		"payments": map[string]interface{}{
			"amount":      amountPence,
			"currency":    "GBP",
			"description": description,
			"links": map[string]string{
				"mandate": mandateID,
			},
		},
	}

	var resp struct {
		Payments struct {
			ID string `json:"id"`
		} `json:"payments"`
	}
	if err := c.doRequest(ctx, http.MethodPost, "/payments", idempotencyKey, payload, &resp); err != nil {
		return "", err
	}
	return resp.Payments.ID, nil
}

// UpdateSubscriptionAmount updates the recurring monthly amount on an existing GoCardless subscription.
func (c *GoCardlessClient) UpdateSubscriptionAmount(ctx context.Context, subscriptionID string, amountPence int, planName string) error {
	payload := map[string]interface{}{
		"subscriptions": map[string]interface{}{
			"amount": amountPence,
			"name":   fmt.Sprintf("VibeBackr %s Plan", strings.Title(planName)),
		},
	}
	return c.doRequest(ctx, http.MethodPut, "/subscriptions/"+subscriptionID, "", payload, nil)
}

// CancelSubscription cancels an active GoCardless subscription.
func (c *GoCardlessClient) CancelSubscription(ctx context.Context, subscriptionID string) error {
	if subscriptionID == "" || !c.IsConfigured() {
		return nil
	}
	payload := map[string]interface{}{
		"data": map[string]interface{}{},
	}
	return c.doRequest(ctx, http.MethodPost, "/subscriptions/"+subscriptionID+"/actions/cancel", "", payload, nil)
}

// VerifyWebhookSignature validates the GoCardless Webhook-Signature header using HMAC-SHA256.
func (c *GoCardlessClient) VerifyWebhookSignature(payload []byte, signatureHeader string) bool {
	if c == nil || c.webhookSecret == "" {
		return false
	}
	sig := strings.TrimSpace(signatureHeader)
	if sig == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(c.webhookSecret))
	mac.Write(payload)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(strings.ToLower(sig)), []byte(strings.ToLower(expected)))
}
