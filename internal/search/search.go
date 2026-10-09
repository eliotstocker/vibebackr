package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/eliotstocker/vibebackr/internal/db"
	"github.com/eliotstocker/vibebackr/internal/domain"
	"github.com/eliotstocker/vibebackr/internal/repository"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

const (
	embeddingDim       = 768
	minVectorScore     = 0.05
	backfillBatchLimit = 200
)

// SearchResult wraps a found record along with an optional relevance score.
type SearchResult struct {
	Record domain.Record `json:"record"`
	Score  float64       `json:"score"`
	Method string        `json:"method"` // "vector" or "sql_fallback"
}

// Embedder generates 768-dimensional vector representations.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// OllamaGemmaEmbedder calls an Ollama instance serving EmbeddingGemma 2 (or compatible 768d model).
type OllamaGemmaEmbedder struct {
	Endpoint string
	Model    string
	Client   *http.Client
}

// NewOllamaGemmaEmbedder creates an embedder pointing to an Ollama or OpenAI-compatible endpoint.
func NewOllamaGemmaEmbedder(endpoint, model string) *OllamaGemmaEmbedder {
	if endpoint == "" {
		endpoint = "http://localhost:11434"
	}
	if model == "" {
		model = "embeddinggemma2"
	}
	return &OllamaGemmaEmbedder{
		Endpoint: strings.TrimRight(endpoint, "/"),
		Model:    model,
		Client:   &http.Client{Timeout: 10 * time.Second},
	}
}

func (o *OllamaGemmaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	reqBody, err := json.Marshal(map[string]interface{}{
		"model":  o.Model,
		"prompt": text,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", o.Endpoint+"/api/embeddings", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedder request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedder returned status: %d", resp.StatusCode)
	}

	var res struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}

	if len(res.Embedding) != embeddingDim {
		return nil, fmt.Errorf("unexpected embedding dimension %d (expected %d)", len(res.Embedding), embeddingDim)
	}

	return res.Embedding, nil
}

// GeminiEmbedder calls the Google Gemini / Vertex AI Express Embedding API (e.g. gemini-embedding-001) with 768 output dimensions.
type GeminiEmbedder struct {
	APIKey   string
	Model    string
	Endpoint string
	Client   *http.Client
}

// NewGeminiEmbedder creates an embedder backed by Google's Generative Language or Vertex AI Express API.
func NewGeminiEmbedder(apiKey, model string) *GeminiEmbedder {
	if model == "" {
		model = "gemini-embedding-001"
	}
	model = strings.TrimPrefix(model, "models/")
	trimmedKey := strings.TrimSpace(apiKey)
	endpoint := "https://generativelanguage.googleapis.com"
	if strings.HasPrefix(trimmedKey, "AQ.") {
		endpoint = "https://aiplatform.googleapis.com"
	}
	return &GeminiEmbedder{
		APIKey:   trimmedKey,
		Model:    model,
		Endpoint: endpoint,
		Client:   &http.Client{Timeout: 10 * time.Second},
	}
}

func (g *GeminiEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	isVertex := strings.Contains(g.Endpoint, "aiplatform.googleapis.com")
	vals, err := g.doEmbedRequest(ctx, text, isVertex)
	if err != nil && isVertex && g.Endpoint == "https://aiplatform.googleapis.com" {
		// Fallback to generativelanguage.googleapis.com if AQ. key is scoped to AI Studio
		if altVals, altErr := g.doEmbedRequestWithBase(ctx, text, "https://generativelanguage.googleapis.com", false); altErr == nil {
			vals = altVals
			err = nil
		}
	}
	if err != nil {
		return nil, err
	}

	if len(vals) > embeddingDim {
		vals = vals[:embeddingDim]
	}
	if len(vals) != embeddingDim {
		return nil, fmt.Errorf("unexpected gemini embedding dimension %d (expected %d)", len(vals), embeddingDim)
	}

	// L2-normalize truncated Matryoshka (768d) embeddings as recommended by Gemini docs
	var normSq float64
	for _, v := range vals {
		fv := float64(v)
		normSq += fv * fv
	}
	if normSq > 0 {
		invNorm := float32(1.0 / math.Sqrt(normSq))
		for i := range vals {
			vals[i] *= invNorm
		}
	}

	return vals, nil
}

func (g *GeminiEmbedder) doEmbedRequest(ctx context.Context, text string, isVertex bool) ([]float32, error) {
	return g.doEmbedRequestWithBase(ctx, text, g.Endpoint, isVertex)
}

func (g *GeminiEmbedder) doEmbedRequestWithBase(ctx context.Context, text, baseEndpoint string, isVertex bool) ([]float32, error) {
	var reqBody []byte
	var err error
	var url string

	base := strings.TrimRight(baseEndpoint, "/")
	if isVertex {
		reqBody, err = json.Marshal(map[string]interface{}{
			"instances": []map[string]interface{}{
				{"content": text},
			},
			"parameters": map[string]interface{}{
				"outputDimensionality": embeddingDim,
			},
		})
		url = fmt.Sprintf("%s/v1/publishers/google/models/%s:predict", base, g.Model)
	} else {
		reqBody, err = json.Marshal(map[string]interface{}{
			"model": "models/" + g.Model,
			"content": map[string]interface{}{
				"parts": []map[string]string{
					{"text": text},
				},
			},
			"outputDimensionality": embeddingDim,
		})
		url = fmt.Sprintf("%s/v1beta/models/%s:embedContent", base, g.Model)
	}
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", g.APIKey)

	resp, err := g.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini embedder request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gemini embedder returned status: %d", resp.StatusCode)
	}

	var res struct {
		Embedding struct {
			Values []float32 `json:"values"`
		} `json:"embedding"`
		Predictions []struct {
			Embeddings struct {
				Values []float32 `json:"values"`
			} `json:"embeddings"`
		} `json:"predictions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}

	if len(res.Embedding.Values) > 0 {
		return res.Embedding.Values, nil
	}
	if len(res.Predictions) > 0 && len(res.Predictions[0].Embeddings.Values) > 0 {
		return res.Predictions[0].Embeddings.Values, nil
	}
	return nil, fmt.Errorf("empty embedding values in gemini response")
}


// LocalEmbedder generates deterministic 768-dimensional L2-normalized vectors using
// signed feature hashing over tokens, stems, word bigrams, and character n-grams.
type LocalEmbedder struct{}

// NewLocalEmbedder returns a built-in 768-dimensional embedder requiring no external service.
func NewLocalEmbedder() *LocalEmbedder {
	return &LocalEmbedder{}
}

var stopWords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "by": true, "for": true, "from": true, "in": true, "is": true,
	"it": true, "of": true, "on": true, "or": true, "that": true, "the": true,
	"this": true, "to": true, "was": true, "were": true, "with": true,
}

func tokenizeText(text string) []string {
	raw := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if len(raw) < 1 {
		return nil
	}
	filtered := make([]string, 0, len(raw))
	for _, tok := range raw {
		if !stopWords[tok] {
			filtered = append(filtered, tok)
		}
	}
	if len(filtered) < 1 {
		return raw
	}
	return filtered
}

func stemToken(tok string) string {
	if len(tok) <= 3 {
		return tok
	}
	if strings.HasSuffix(tok, "ies") && len(tok) > 4 {
		return tok[:len(tok)-3] + "y"
	}
	for _, suf := range []string{"tion", "ment", "ness", "able", "ible", "ing", "est", "ed", "es", "er", "ly", "s"} {
		if strings.HasSuffix(tok, suf) && len(tok)-len(suf) >= 3 {
			stem := tok[:len(tok)-len(suf)]
			if len(stem) >= 4 && stem[len(stem)-1] == stem[len(stem)-2] {
				stem = stem[:len(stem)-1]
			}
			return stem
		}
	}
	return tok
}

func addHashedFeature(vec []float64, feat string, weight float64) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(feat))
	sum := h.Sum64()

	idx1 := int(sum % embeddingDim)
	sign1 := 1.0
	if (sum>>32)&1 == 1 {
		sign1 = -1.0
	}
	vec[idx1] += sign1 * weight

	idx2 := int((sum >> 16) % embeddingDim)
	sign2 := 1.0
	if (sum>>48)&1 == 1 {
		sign2 = -1.0
	}
	vec[idx2] += sign2 * weight * 0.5
}

func (l *LocalEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	tokens := tokenizeText(text)
	vec64 := make([]float64, embeddingDim)
	if len(tokens) < 1 {
		return make([]float32, embeddingDim), nil
	}

	wordCounts := make(map[string]int, len(tokens))
	stemCounts := make(map[string]int, len(tokens))
	bigramCounts := make(map[string]int, len(tokens))
	var prevStem string

	for _, tok := range tokens {
		wordCounts[tok]++
		stem := stemToken(tok)
		stemCounts[stem]++
		if prevStem != "" {
			bigramCounts[prevStem+"_"+stem]++
		}
		prevStem = stem
	}

	for tok, cnt := range wordCounts {
		tf := 1.0 + math.Log(float64(cnt))
		addHashedFeature(vec64, "w:"+tok, 2.0*tf)

		runes := []rune("^" + tok + "$")
		if len(runes) >= 4 {
			var ngrams []string
			for n := 3; n <= 4; n++ {
				for i := 0; i+n <= len(runes); i++ {
					ngrams = append(ngrams, string(runes[i:i+n]))
				}
			}
			if len(ngrams) > 0 {
				ngWeight := (0.8 * tf) / math.Sqrt(float64(len(ngrams)))
				for _, ng := range ngrams {
					addHashedFeature(vec64, "g:"+ng, ngWeight)
				}
			}
		}
	}

	for stem, cnt := range stemCounts {
		tf := 1.0 + math.Log(float64(cnt))
		addHashedFeature(vec64, "s:"+stem, 1.5*tf)
	}

	for bg, cnt := range bigramCounts {
		tf := 1.0 + math.Log(float64(cnt))
		addHashedFeature(vec64, "b:"+bg, 1.2*tf)
	}

	var normSq float64
	for _, v := range vec64 {
		normSq += v * v
	}
	out := make([]float32, embeddingDim)
	if normSq > 0 {
		invNorm := 1.0 / math.Sqrt(normSq)
		for i, v := range vec64 {
			out[i] = float32(v * invNorm)
		}
	}
	return out, nil
}

// SearchEngine coordinates RAG vector search with automatic SQL fallback.
type SearchEngine struct {
	db       *db.DB
	repo     repository.Repository
	embedder Embedder
}

// NewSearchEngine creates a unified search engine and hooks record indexing into the repository.
func NewSearchEngine(database *db.DB, repo repository.Repository, embedder Embedder) *SearchEngine {
	if embedder == nil {
		embedder = NewLocalEmbedder()
	}
	se := &SearchEngine{
		db:       database,
		repo:     repo,
		embedder: embedder,
	}
	if repo != nil {
		repo.SetRecordIndexHook(func(ctx context.Context, rec *domain.Record) {
			_ = se.IndexRecord(ctx, rec)
		})
	}
	return se
}

func recordEmbeddingText(rec *domain.Record) string {
	if rec == nil {
		return ""
	}
	var parts []string
	if t := strings.TrimSpace(rec.Title); t != "" {
		parts = append(parts, t)
	}
	if c := strings.TrimSpace(rec.Content); c != "" {
		parts = append(parts, c)
	}
	if d := strings.TrimSpace(rec.Data); d != "" && d != "{}" && d != "null" {
		parts = append(parts, d)
	}
	return strings.Join(parts, "\n\n")
}

func formatVectorLiteral(vec []float32) string {
	vecStrs := make([]string, len(vec))
	for i, v := range vec {
		vecStrs[i] = fmt.Sprintf("%f", v)
	}
	return "[" + strings.Join(vecStrs, ",") + "]"
}

// IndexRecord embeds a record's text and upserts its vector into record_embeddings when running on PostgreSQL.
func (s *SearchEngine) IndexRecord(ctx context.Context, rec *domain.Record) error {
	if s.db == nil || s.db.Driver != db.DriverPostgres || s.embedder == nil || rec == nil || rec.ID == "" {
		return nil
	}
	if s.repo != nil && rec.CollectionID != "" {
		col, err := s.repo.GetCollection(ctx, rec.CollectionID)
		if err != nil || col == nil || !col.VectorSearchEnabled {
			return nil
		}
	}
	return s.indexRecordDirect(ctx, rec)
}

func (s *SearchEngine) indexRecordDirect(ctx context.Context, rec *domain.Record) error {
	chunk := recordEmbeddingText(rec)
	if chunk == "" {
		return nil
	}
	vec, err := s.embedder.Embed(ctx, chunk)
	if err != nil {
		return err
	}
	vecLiteral := formatVectorLiteral(vec)

	_, _ = s.db.ExecContext(ctx, "DELETE FROM record_embeddings WHERE record_id = $1", rec.ID)
	_, err = s.db.ExecContext(
		ctx,
		"INSERT INTO record_embeddings (id, record_id, content_chunk, embedding) VALUES ($1, $2, $3, CAST($4 AS vector))",
		uuid.NewString(), rec.ID, chunk, vecLiteral,
	)
	return err
}

func (s *SearchEngine) backfillCollectionEmbeddings(ctx context.Context, collectionID string) {
	if s.db == nil || s.db.Driver != db.DriverPostgres || s.embedder == nil {
		return
	}
	var unindexed []domain.Record
	err := s.db.SelectContext(ctx, &unindexed, `
		SELECT r.id, r.collection_id, r.title, r.content, r.data
		FROM records r
		WHERE r.collection_id = $1
		  AND NOT EXISTS (SELECT 1 FROM record_embeddings re WHERE re.record_id = r.id)
		LIMIT $2
	`, collectionID, backfillBatchLimit)
	if err != nil {
		return
	}
	for i := range unindexed {
		_ = s.indexRecordDirect(ctx, &unindexed[i])
	}
}

// Search performs hybrid RAG vector search when configured, falling back to SQL text matching.
func (s *SearchEngine) Search(ctx context.Context, collectionID, query string, user *domain.User, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 20
	}

	col, err := s.repo.GetCollection(ctx, collectionID)
	if err != nil {
		return nil, err
	}

	if col.VectorSearchEnabled && s.embedder != nil {
		if s.db.Driver == db.DriverPostgres {
			results, err := s.searchVector(ctx, col.ID, query, user, limit)
			if err == nil && len(results) > 0 {
				return results, nil
			}
		}
		results, err := s.searchVectorInMemory(ctx, col.ID, query, user, limit)
		if err == nil && len(results) > 0 {
			return results, nil
		}
	}

	return s.searchSQL(ctx, col.ID, query, user, limit)
}

func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) < 1 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		va := float64(a[i])
		vb := float64(b[i])
		dot += va * vb
		normA += va * va
		normB += vb * vb
	}
	if normA <= 0 || normB <= 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

func (s *SearchEngine) searchVectorInMemory(ctx context.Context, collectionID, query string, user *domain.User, limit int) ([]SearchResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	qVec, err := s.embedder.Embed(ctx, query)
	if err != nil {
		return nil, err
	}
	records, err := s.repo.ListRecords(ctx, collectionID, user)
	if err != nil {
		return nil, err
	}

	var results []SearchResult
	for i := range records {
		chunk := recordEmbeddingText(&records[i])
		if chunk == "" {
			continue
		}
		rVec, err := s.embedder.Embed(ctx, chunk)
		if err != nil {
			continue
		}
		score := cosineSimilarity(qVec, rVec)
		if score > minVectorScore {
			results = append(results, SearchResult{
				Record: records[i],
				Score:  score,
				Method: "vector",
			})
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

func (s *SearchEngine) searchVector(ctx context.Context, collectionID, query string, user *domain.User, limit int) ([]SearchResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	s.backfillCollectionEmbeddings(ctx, collectionID)

	vec, err := s.embedder.Embed(ctx, query)
	if err != nil {
		return nil, err
	}
	vecLiteral := formatVectorLiteral(vec)

	var jsonVisCondition = `(
		(CASE WHEN r.access_policy IS NOT NULL AND r.access_policy != '' THEN CAST(r.access_policy AS jsonb)->>'visibility' ELSE 'private' END = 'public')
		AND (
			c.tenant_id = ''
			OR c.tenant_id = :user_tenant_id
			OR EXISTS (SELECT 1 FROM user_tenants ut WHERE ut.user_id = :user_id AND ut.tenant_id = c.tenant_id)
			OR (:user_domain != '' AND CASE WHEN c.allowed_domains IS NOT NULL AND c.allowed_domains != '' AND jsonb_typeof(CAST(c.allowed_domains AS jsonb)) = 'array' THEN EXISTS (SELECT 1 FROM jsonb_array_elements_text(CAST(c.allowed_domains AS jsonb)) WHERE LOWER(value) = LOWER(:user_domain)) ELSE false END)
		)
	)`
	var jsonShareCondition = "CASE WHEN r.access_policy IS NOT NULL AND r.access_policy != '' AND jsonb_typeof(CAST(r.access_policy AS jsonb)->'shares') = 'array' THEN EXISTS (SELECT 1 FROM jsonb_array_elements_text(CAST(r.access_policy AS jsonb)->'shares') WHERE LOWER(value) = LOWER(:user_email)) ELSE false END"

	sqlQuery := fmt.Sprintf(`
	WITH RECURSIVE org_edges AS (
		SELECT id, manager_id, COALESCE(tenant_id, '') AS tenant_id FROM users
		UNION ALL
		SELECT user_id AS id, manager_id, COALESCE(tenant_id, '') AS tenant_id FROM user_tenants
	),
	subordinates AS (
		SELECT id, tenant_id FROM org_edges WHERE manager_id = :user_id
		UNION
		SELECT e.id, e.tenant_id FROM org_edges e JOIN subordinates sub ON e.manager_id = sub.id AND (e.tenant_id = '' OR sub.tenant_id = '' OR e.tenant_id = sub.tenant_id)
	),
	ranked AS (
		SELECT DISTINCT ON (r.id)
			r.id, r.collection_id, r.title, r.content, r.data, r.created_by, r.access_policy, r.created_at, r.updated_at,
			1 - (re.embedding <=> CAST('%s' AS vector)) AS score
		FROM record_embeddings re
		JOIN records r ON re.record_id = r.id
		JOIN collections c ON r.collection_id = c.id
		WHERE r.collection_id = :collection_id
		  AND (
			(:user_role = 'admin' AND c.tenant_id = :user_tenant_id)
			OR (c.tenant_id != '' AND EXISTS (SELECT 1 FROM user_tenants ut WHERE ut.user_id = :user_id AND ut.tenant_id = c.tenant_id AND ut.role = 'admin'))
			OR r.created_by = :user_id
			OR EXISTS (SELECT 1 FROM subordinates sub WHERE sub.id = r.created_by AND (c.tenant_id = '' OR sub.tenant_id = c.tenant_id))
			OR %s
			OR %s
		  )
		ORDER BY r.id, score DESC
	)
	SELECT id, collection_id, title, content, data, created_by, access_policy, created_at, updated_at, score
	FROM ranked
	WHERE score > %f
	ORDER BY score DESC
	LIMIT :limit
	`, vecLiteral, jsonVisCondition, jsonShareCondition, minVectorScore)

	userTenantID := ""
	if user.TenantID != nil {
		userTenantID = *user.TenantID
	}
	userDomain := ""
	if parts := strings.Split(user.Email, "@"); len(parts) == 2 {
		userDomain = parts[1]
	}

	params := map[string]interface{}{
		"user_id":        user.ID,
		"user_email":     user.Email,
		"user_domain":    userDomain,
		"user_role":      user.Role,
		"user_tenant_id": userTenantID,
		"collection_id":  collectionID,
		"limit":          limit,
	}

	namedQuery, args, err := sqlx.Named(sqlQuery, params)
	if err != nil {
		return nil, err
	}
	namedQuery = s.db.Rebind(namedQuery)

	type vecRow struct {
		domain.Record
		Score float64 `db:"score"`
	}

	var rows []vecRow
	if err := s.db.SelectContext(ctx, &rows, namedQuery, args...); err != nil {
		return nil, err
	}

	results := make([]SearchResult, len(rows))
	for i, row := range rows {
		results[i] = SearchResult{
			Record: row.Record,
			Score:  row.Score,
			Method: "vector",
		}
	}

	return results, nil
}

func (s *SearchEngine) searchSQL(ctx context.Context, collectionID, query string, user *domain.User, limit int) ([]SearchResult, error) {
	likeQuery := "%" + query + "%"

	var textMatch string
	var jsonVisCondition string
	var jsonShareCondition string

	if s.db.Driver == db.DriverSQLite {
		textMatch = "(r.title LIKE :like_query OR r.content LIKE :like_query)"
		jsonVisCondition = "json_extract(r.access_policy, '$.visibility') = 'public'"
		jsonShareCondition = "EXISTS (SELECT 1 FROM json_each(json_extract(r.access_policy, '$.shares')) WHERE LOWER(value) = LOWER(:user_email))"
	} else {
		textMatch = "(r.title ILIKE :like_query OR r.content ILIKE :like_query)"
		jsonVisCondition = "(CASE WHEN r.access_policy IS NOT NULL AND r.access_policy != '' THEN CAST(r.access_policy AS jsonb)->>'visibility' ELSE 'private' END = 'public')"
		jsonShareCondition = "CASE WHEN r.access_policy IS NOT NULL AND r.access_policy != '' AND jsonb_typeof(CAST(r.access_policy AS jsonb)->'shares') = 'array' THEN EXISTS (SELECT 1 FROM jsonb_array_elements_text(CAST(r.access_policy AS jsonb)->'shares') WHERE LOWER(value) = LOWER(:user_email)) ELSE false END"
	}

	sqlQuery := fmt.Sprintf(`
	WITH RECURSIVE org_edges AS (
		SELECT id, manager_id, COALESCE(tenant_id, '') AS tenant_id FROM users
		UNION ALL
		SELECT user_id AS id, manager_id, COALESCE(tenant_id, '') AS tenant_id FROM user_tenants
	),
	subordinates AS (
		SELECT id, tenant_id FROM org_edges WHERE manager_id = :user_id
		UNION
		SELECT e.id, e.tenant_id FROM org_edges e JOIN subordinates s ON e.manager_id = s.id AND (e.tenant_id = '' OR s.tenant_id = '' OR e.tenant_id = s.tenant_id)
	)
	SELECT r.id, r.collection_id, r.title, r.content, r.data, r.created_by, r.access_policy, r.created_at, r.updated_at
	FROM records r
	JOIN collections c ON r.collection_id = c.id
	WHERE r.collection_id = :collection_id
	  AND %s
	  AND (
		(:user_role = 'admin' AND c.tenant_id = :user_tenant_id)
		OR (c.tenant_id != '' AND EXISTS (SELECT 1 FROM user_tenants ut WHERE ut.user_id = :user_id AND ut.tenant_id = c.tenant_id AND ut.role = 'admin'))
		OR r.created_by = :user_id
		OR EXISTS (SELECT 1 FROM subordinates sub WHERE sub.id = r.created_by AND (c.tenant_id = '' OR sub.tenant_id = c.tenant_id))
		OR %s
		OR %s
	  )
	ORDER BY r.created_at DESC
	LIMIT :limit
	`, textMatch, jsonVisCondition, jsonShareCondition)

	userTenantID := ""
	if user.TenantID != nil {
		userTenantID = *user.TenantID
	}

	params := map[string]interface{}{
		"user_id":        user.ID,
		"user_email":     user.Email,
		"user_role":      user.Role,
		"user_tenant_id": userTenantID,
		"collection_id":  collectionID,
		"like_query":     likeQuery,
		"limit":          limit,
	}

	namedQuery, args, err := sqlx.Named(sqlQuery, params)
	if err != nil {
		return nil, fmt.Errorf("failed binding SQL search: %w", err)
	}
	namedQuery = s.db.Rebind(namedQuery)

	var records []domain.Record
	if err := s.db.SelectContext(ctx, &records, namedQuery, args...); err != nil {
		return nil, err
	}

	results := make([]SearchResult, len(records))
	for i, r := range records {
		results[i] = SearchResult{
			Record: r,
			Score:  1.0,
			Method: "sql_fallback",
		}
	}

	return results, nil
}
