package search_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eliotstocker/vibebackr/internal/db"
	"github.com/eliotstocker/vibebackr/internal/domain"
	"github.com/eliotstocker/vibebackr/internal/repository"
	"github.com/eliotstocker/vibebackr/internal/search"
	"github.com/jmoiron/sqlx"
)

type mockEmbedder struct {
	vec []float32
}

func (m *mockEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	return m.vec, nil
}

func TestSearchEngineSQLFallback(t *testing.T) {
	ctx := context.Background()
	database, err := db.Connect("sqlite", "file:searchtest?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("db error: %v", err)
	}

	repo := repository.New(database)
	searchEngine := search.NewSearchEngine(database, repo, nil)

	user := &domain.User{
		ID:    "user_search",
		Email: "search@test.com",
		Role:  domain.RoleMember,
	}
	_ = repo.CreateUser(ctx, user)

	col := &domain.Collection{
		ID:                "col_search",
		Name:              "Docs",
		DefaultVisibility: domain.VisibilityPublic,
	}
	_ = repo.CreateCollection(ctx, col)

	rec1 := &domain.Record{
		CollectionID: col.ID,
		Title:        "Go Concurrency Patterns",
		Content:      "Goroutines and channels make parallel programming elegant.",
		CreatedBy:    user.ID,
		AccessPolicy: domain.AccessPolicy{Visibility: domain.VisibilityPublic},
	}
	rec2 := &domain.Record{
		CollectionID: col.ID,
		Title:        "Python Basics",
		Content:      "Learn indentation and dynamic typing.",
		CreatedBy:    user.ID,
		AccessPolicy: domain.AccessPolicy{Visibility: domain.VisibilityPublic},
	}
	_ = repo.CreateRecord(ctx, rec1)
	_ = repo.CreateRecord(ctx, rec2)

	// Search matching title
	results, err := searchEngine.Search(ctx, col.ID, "Concurrency", user, 5)
	if err != nil {
		t.Fatalf("search error: %v", err)
	}
	if len(results) != 1 || results[0].Record.Title != "Go Concurrency Patterns" {
		t.Fatalf("unexpected search results: %+v", results)
	}
	if results[0].Method != "sql_fallback" {
		t.Errorf("expected sql_fallback method, got %s", results[0].Method)
	}

	// Search matching content
	resultsContent, err := searchEngine.Search(ctx, col.ID, "parallel", user, 5)
	if err != nil || len(resultsContent) != 1 {
		t.Fatalf("expected content search hit: %v", resultsContent)
	}

	// Search non-matching
	resultsEmpty, err := searchEngine.Search(ctx, col.ID, "NonExistentKeyword", user, 5)
	if err != nil {
		t.Fatalf("search error: %v", err)
	}
	if len(resultsEmpty) != 0 {
		t.Fatalf("expected 0 results, got %d", len(resultsEmpty))
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestOllamaGemmaEmbedder(t *testing.T) {
	dummyVec := make([]float32, 768)
	for i := range dummyVec {
		dummyVec[i] = 0.01 * float32(i)
	}
	respJSON, _ := json.Marshal(map[string]interface{}{
		"embedding": dummyVec,
	})

	embedder := search.NewOllamaGemmaEmbedder("http://mock-ollama:11434", "embeddinggemma2")
	embedder.Client.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		rec.Header().Set("Content-Type", "application/json")
		rec.WriteHeader(http.StatusOK)
		_, _ = rec.Write(respJSON)
		return rec.Result(), nil
	})

	vec, err := embedder.Embed(context.Background(), "hello autonomous world")
	if err != nil {
		t.Fatalf("failed embedding: %v", err)
	}
	if len(vec) != 768 {
		t.Fatalf("expected 768 dimensions, got %d", len(vec))
	}
}

func TestGeminiEmbedder(t *testing.T) {
	dummyVec := make([]float32, 768)
	for i := range dummyVec {
		dummyVec[i] = float32(i + 1)
	}
	respJSON, _ := json.Marshal(map[string]interface{}{
		"embedding": map[string]interface{}{
			"values": dummyVec,
		},
	})

	embedder := search.NewGeminiEmbedder("test-gemini-key", "gemini-embedding-001")
	embedder.Client.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("x-goog-api-key") != "test-gemini-key" {
			t.Fatalf("expected x-goog-api-key header, got %q", req.Header.Get("x-goog-api-key"))
		}
		if !strings.HasSuffix(req.URL.Path, "/v1beta/models/gemini-embedding-001:embedContent") {
			t.Fatalf("unexpected request path: %s", req.URL.Path)
		}
		rec := httptest.NewRecorder()
		rec.Header().Set("Content-Type", "application/json")
		rec.WriteHeader(http.StatusOK)
		_, _ = rec.Write(respJSON)
		return rec.Result(), nil
	})

	vec, err := embedder.Embed(context.Background(), "hello gemini embeddings")
	if err != nil {
		t.Fatalf("failed gemini embedding: %v", err)
	}
	if len(vec) != 768 {
		t.Fatalf("expected 768 dimensions, got %d", len(vec))
	}

	vertexRespJSON, _ := json.Marshal(map[string]interface{}{
		"predictions": []map[string]interface{}{
			{
				"embeddings": map[string]interface{}{
					"values": dummyVec,
				},
			},
		},
	})
	vertexEmbedder := search.NewGeminiEmbedder("AQ.test-vertex-express-key", "gemini-embedding-001")
	vertexEmbedder.Client.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("x-goog-api-key") != "AQ.test-vertex-express-key" {
			t.Fatalf("expected AQ. x-goog-api-key header, got %q", req.Header.Get("x-goog-api-key"))
		}
		if !strings.HasSuffix(req.URL.Path, "/v1/publishers/google/models/gemini-embedding-001:predict") {
			t.Fatalf("unexpected vertex request path: %s", req.URL.Path)
		}
		rec := httptest.NewRecorder()
		rec.Header().Set("Content-Type", "application/json")
		rec.WriteHeader(http.StatusOK)
		_, _ = rec.Write(vertexRespJSON)
		return rec.Result(), nil
	})
	vVec, err := vertexEmbedder.Embed(context.Background(), "hello vertex express embeddings")
	if err != nil {
		t.Fatalf("failed vertex express embedding: %v", err)
	}
	if len(vVec) != 768 {
		t.Fatalf("expected 768 dimensions, got %d", len(vVec))
	}
}

func TestPostgresSearchQueryNamedCompiles(t *testing.T) {
	textMatch := "(r.title ILIKE :like_query OR r.content ILIKE :like_query)"
	jsonVisCondition := "(CASE WHEN r.access_policy IS NOT NULL AND r.access_policy != '' THEN CAST(r.access_policy AS jsonb)->>'visibility' ELSE 'private' END = 'public')"
	jsonShareCondition := "CASE WHEN r.access_policy IS NOT NULL AND r.access_policy != '' AND jsonb_typeof(CAST(r.access_policy AS jsonb)->'shares') = 'array' THEN EXISTS (SELECT 1 FROM jsonb_array_elements_text(CAST(r.access_policy AS jsonb)->'shares') WHERE value = :user_email) ELSE false END"

	sqlQuery := fmt.Sprintf(`
	WITH RECURSIVE subordinates AS (
		SELECT id FROM users WHERE manager_id = :user_id
		UNION ALL
		SELECT u.id FROM users u JOIN subordinates s ON u.manager_id = s.id
	)
	SELECT r.id, r.collection_id, r.title, r.content, r.data, r.created_by, r.access_policy, r.created_at, r.updated_at
	FROM records r
	JOIN collections c ON r.collection_id = c.id
	WHERE r.collection_id = :collection_id
	  AND %s
	  AND (
		:user_role = 'admin'
		OR r.created_by = :user_id
		OR r.created_by IN (SELECT id FROM subordinates)
		OR %s
		OR c.default_visibility = 'public'
		OR %s
	  )
	ORDER BY r.created_at DESC
	LIMIT :limit
	`, textMatch, jsonVisCondition, jsonShareCondition)

	params := map[string]interface{}{
		"user_id":       "user-1",
		"user_email":    "test@example.com",
		"user_role":     "member",
		"collection_id": "col-1",
		"like_query":    "%term%",
		"limit":         10,
	}

	boundQuery, _, err := sqlx.Named(sqlQuery, params)
	if err != nil {
		t.Fatalf("Named query compilation failed: %v", err)
	}
	if strings.Contains(boundQuery, ":jsonb") || strings.Contains(boundQuery, ":vector") {
		t.Fatalf("boundQuery contains mangled sqlx colon artifact: %s", boundQuery)
	}
	rebound := sqlx.Rebind(sqlx.DOLLAR, boundQuery)
	if strings.Contains(rebound, ":jsonb") {
		t.Fatalf("rebound contains :jsonb: %s", rebound)
	}
}

func TestVectorSearchScoresAndOrdering(t *testing.T) {
	ctx := context.Background()
	database, err := db.Connect("sqlite", "file:vecsearchtest?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("db error: %v", err)
	}

	repo := repository.New(database)
	searchEngine := search.NewSearchEngine(database, repo, nil)

	user := &domain.User{
		ID:    "user_vec",
		Email: "vec@test.com",
		Role:  domain.RoleMember,
	}
	_ = repo.CreateUser(ctx, user)

	col := &domain.Collection{
		ID:                  "col_vec",
		Name:                "Vector Docs",
		DefaultVisibility:   domain.VisibilityPublic,
		VectorSearchEnabled: true,
	}
	_ = repo.CreateCollection(ctx, col)

	recPartial := &domain.Record{
		ID:           "00000000-0000-0000-0000-000000000001",
		CollectionID: col.ID,
		Title:        "Go Web Servers",
		Content:      "Building HTTP services and middleware with standard library.",
		CreatedBy:    user.ID,
		AccessPolicy: domain.AccessPolicy{Visibility: domain.VisibilityPublic},
	}
	recBest := &domain.Record{
		ID:           "ffffffff-ffff-ffff-ffff-ffffffffffff",
		CollectionID: col.ID,
		Title:        "Go Concurrency Patterns",
		Content:      "Goroutines and channels make concurrent programming in Go fast and reliable.",
		CreatedBy:    user.ID,
		AccessPolicy: domain.AccessPolicy{Visibility: domain.VisibilityPublic},
	}
	_ = repo.CreateRecord(ctx, recPartial)
	_ = repo.CreateRecord(ctx, recBest)

	results, err := searchEngine.Search(ctx, col.ID, "Go goroutine concurrency", user, 5)
	if err != nil {
		t.Fatalf("vector search error: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("expected at least 2 vector results, got %d", len(results))
	}
	if results[0].Method != "vector" {
		t.Fatalf("expected method 'vector', got %s", results[0].Method)
	}
	if results[0].Record.ID != recBest.ID {
		t.Fatalf("expected best match %s first, got %s", recBest.ID, results[0].Record.ID)
	}
	if results[0].Score <= results[1].Score {
		t.Fatalf("expected descending scores, got %f <= %f", results[0].Score, results[1].Score)
	}
	if results[0].Score >= 0.999 {
		t.Fatalf("expected realistic partial cosine similarity (< 1.0), got %f", results[0].Score)
	}
}


