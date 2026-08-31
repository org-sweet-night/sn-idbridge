// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/smices/open-idb/internal/auth"
	"github.com/smices/open-idb/internal/clientsecret"
	"github.com/smices/open-idb/internal/db/generated"
	"github.com/smices/open-idb/internal/httpserver"
	"github.com/smices/open-idb/internal/sso"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestOIDCAuthorizationCodeFlow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	conn := os.Getenv("OPEN_IDB_TEST_DATABASE_URL")
	if conn == "" {
		testcontainers.SkipIfProviderIsNotHealthy(t)
		container, err := postgres.Run(ctx,
			"postgres:16-alpine",
			postgres.WithDatabase("idbridge"),
			postgres.WithUsername("postgres"),
			postgres.WithPassword("postgres"),
			testcontainers.WithWaitStrategy(wait.ForListeningPort("5432/tcp")),
		)
		if err != nil {
			t.Fatalf("start postgres container: %v", err)
		}
		t.Cleanup(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cleanupCancel()
			if err := container.Terminate(cleanupCtx); err != nil {
				t.Errorf("terminate postgres container: %v", err)
			}
		})
		conn, err = container.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			t.Fatalf("connection string: %v", err)
		}
	}
	applyMigrations(ctx, t, conn)

	pool, err := pgxpool.New(ctx, conn)
	if err != nil {
		t.Fatalf("open pgx pool: %v", err)
	}
	t.Cleanup(pool.Close)

	queries := generated.New(pool)
	entity, user := createOIDCTestIdentity(ctx, t, queries)
	clientID := "idb-console"
	redirectURI := "https://app.example.test/callback"
	app := createOIDCTestClient(ctx, t, queries, entity.ID, clientID, redirectURI)
	insertApplicationAssignment(ctx, t, pool, entity.ID, app.ID, "user", user.ID, "allow")

	privateKey, err := sso.GenerateRSAKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	service, err := sso.NewService(sso.ServiceConfig{
		Issuer:         "https://idb.example.test",
		KeyID:          "dev-key-1",
		PrivateKey:     privateKey,
		Store:          queries,
		AuthCodeTTL:    5 * time.Minute,
		AccessTokenTTL: 15 * time.Minute,
		IDTokenTTL:     15 * time.Minute,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	handler := sso.NewHandler(service)
	server := httptest.NewServer(httpserver.NewRouter(handler.RegisterRoutes))
	t.Cleanup(server.Close)

	getJSON(t, server.URL+"/.well-known/openid-configuration", http.StatusOK)
	getJSON(t, server.URL+"/.well-known/jwks.json", http.StatusOK)

	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	challenge := s256Challenge(verifier)

	localUser := createOIDCTestLocalManagementUser(ctx, t, queries, entity.ID)
	insertApplicationAssignment(ctx, t, pool, entity.ID, app.ID, "user", localUser.ID, "allow")
	localAuthorize := authorizeRequest(ctx, t, server, entity.ID, localUser.ID, clientID, redirectURI, challenge, "admin", "Admin")
	_, _ = io.ReadAll(localAuthorize.Body)
	localAuthorize.Body.Close()
	if localAuthorize.StatusCode != http.StatusFound {
		t.Fatalf("local authorize status = %d, want %d", localAuthorize.StatusCode, http.StatusFound)
	}
	if location := localAuthorize.Header.Get("Location"); !strings.Contains(location, "/login") || !strings.Contains(location, "return_to") {
		t.Fatalf("local authorize location = %q, want login redirect with return_to", location)
	}
	cleared := false
	for _, cookie := range localAuthorize.Cookies() {
		if cookie.Name == "idb_session" && cookie.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("local authorize did not clear idb_session")
	}

	code := authorizeCode(ctx, t, server, entity.ID, user.ID, clientID, redirectURI, challenge)

	tokenBody := url.Values{}
	tokenBody.Set("grant_type", "authorization_code")
	tokenBody.Set("client_id", clientID)
	tokenBody.Set("client_secret", "secret-1")
	tokenBody.Set("code", code)
	tokenBody.Set("redirect_uri", redirectURI)
	tokenBody.Set("code_verifier", verifier)
	tokenResponse := postForm(t, server.URL+"/oauth2/token", tokenBody, http.StatusOK)

	if tokenResponse["token_type"] != "Bearer" {
		t.Fatalf("token_type = %#v, want Bearer", tokenResponse["token_type"])
	}
	if tokenResponse["expires_in"].(float64) <= 0 {
		t.Fatalf("expires_in = %#v, want positive", tokenResponse["expires_in"])
	}
	if tokenResponse["scope"] != "openid email" {
		t.Fatalf("scope = %#v, want requested scope only", tokenResponse["scope"])
	}
	parsedIDToken, _, err := jwt.NewParser().ParseUnverified(tokenResponse["id_token"].(string), jwt.MapClaims{})
	if err != nil {
		t.Fatalf("parse id token: %v", err)
	}
	if parsedIDToken.Claims.(jwt.MapClaims)["nonce"] != "nonce-1" {
		t.Fatalf("nonce = %#v, want nonce-1", parsedIDToken.Claims.(jwt.MapClaims)["nonce"])
	}
	for _, key := range []string{"access_token", "id_token"} {
		token, ok := tokenResponse[key].(string)
		if !ok || strings.Count(token, ".") != 2 {
			t.Fatalf("%s = %#v, want signed-looking JWT", key, tokenResponse[key])
		}
	}

	reuseResponse := postForm(t, server.URL+"/oauth2/token", tokenBody, http.StatusBadRequest)
	if reuseResponse["error"] != "invalid_grant" {
		t.Fatalf("reuse error = %#v, want invalid_grant", reuseResponse)
	}

	// Simulate a client row that existed before 000008. Migrated clients keep
	// the legacy no-secret exchange behavior until explicitly recreated.
	if _, err := pool.Exec(ctx, `UPDATE oidc_clients SET secret_required = false WHERE entity_id = $1 AND client_id = $2`, entity.ID, clientID); err != nil {
		t.Fatalf("mark client as migrated legacy client: %v", err)
	}
	legacyCode := authorizeCode(ctx, t, server, entity.ID, user.ID, clientID, redirectURI, challenge)
	legacyTokenBody := url.Values{}
	legacyTokenBody.Set("grant_type", "authorization_code")
	legacyTokenBody.Set("client_id", clientID)
	legacyTokenBody.Set("code", legacyCode)
	legacyTokenBody.Set("redirect_uri", redirectURI)
	legacyTokenBody.Set("code_verifier", verifier)
	postForm(t, server.URL+"/oauth2/token", legacyTokenBody, http.StatusOK)

	disabledCode := authorizeCode(ctx, t, server, entity.ID, user.ID, clientID, redirectURI, challenge)
	if _, err := pool.Exec(ctx, `UPDATE applications SET status = 'disabled' WHERE entity_id = $1 AND id = $2`, entity.ID, app.ID); err != nil {
		t.Fatalf("disable application: %v", err)
	}
	disabledTokenBody := url.Values{}
	disabledTokenBody.Set("grant_type", "authorization_code")
	disabledTokenBody.Set("client_id", clientID)
	disabledTokenBody.Set("client_secret", "secret-1")
	disabledTokenBody.Set("code", disabledCode)
	disabledTokenBody.Set("redirect_uri", redirectURI)
	disabledTokenBody.Set("code_verifier", verifier)
	disabledResponse := postForm(t, server.URL+"/oauth2/token", disabledTokenBody, http.StatusBadRequest)
	if disabledResponse["error"] != "invalid_grant" {
		t.Fatalf("disabled application error = %#v, want invalid_grant", disabledResponse)
	}
	var codeWasNotConsumed bool
	if err := pool.QueryRow(ctx, `SELECT used_at IS NULL FROM oauth_authorization_codes WHERE entity_id = $1 AND code_hash = $2`, entity.ID, sso.HashToken(disabledCode)).Scan(&codeWasNotConsumed); err != nil {
		t.Fatalf("read disabled application authorization code: %v", err)
	}
	if !codeWasNotConsumed {
		t.Fatal("disabled application authorization code was consumed")
	}
}

func createOIDCTestIdentity(ctx context.Context, t *testing.T, queries *generated.Queries) (generated.BusinessEntity, generated.User) {
	t.Helper()
	entity, err := queries.CreateEntity(ctx, generated.CreateEntityParams{
		Name:          "OIDC Entity",
		Slug:          "oidc",
		DefaultLocale: "en-US",
	})
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	source, err := queries.CreateIdentitySource(ctx, generated.CreateIdentitySourceParams{
		EntityID:    entity.ID,
		Type:        "feishu",
		Name:        "Feishu",
		SyncEnabled: true,
	})
	if err != nil {
		t.Fatalf("create identity source: %v", err)
	}
	directoryUser, err := queries.UpsertDirectoryUser(ctx, generated.UpsertDirectoryUserParams{
		EntityID:       entity.ID,
		SourceID:       source.ID,
		ExternalUserID: "ou_ada",
		Name:           "Ada Lovelace",
		Email:          pgtype.Text{String: "ada@example.test", Valid: true},
		Status:         "active",
		RawProfile:     []byte(`{"source":"test"}`),
	})
	if err != nil {
		t.Fatalf("create directory user: %v", err)
	}
	user, err := queries.CreateManagedUser(ctx, generated.CreateManagedUserParams{
		EntityID:        entity.ID,
		Username:        "ada",
		DisplayName:     "Ada Lovelace",
		Email:           pgtype.Text{String: "ada@example.test", Valid: true},
		LifecycleStatus: "active",
		UserType:        "employee",
		PrimarySourceID: pgtype.Text{String: source.ID, Valid: true},
		Locale:          pgtype.Text{String: "en-US", Valid: true},
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := queries.CreateAccountBinding(ctx, generated.CreateAccountBindingParams{
		EntityID:        entity.ID,
		UserID:          user.ID,
		SourceID:        source.ID,
		DirectoryUserID: directoryUser.ID,
		ProviderUid:     "ou_ada",
		ProviderUnionID: pgtype.Text{String: "on_ada", Valid: true},
		IsPrimary:       true,
	}); err != nil {
		t.Fatalf("create account binding: %v", err)
	}
	return entity, user
}

func createOIDCTestLocalManagementUser(ctx context.Context, t *testing.T, queries *generated.Queries, entityID string) generated.User {
	t.Helper()
	source, err := queries.CreateIdentitySource(ctx, generated.CreateIdentitySourceParams{
		EntityID:    entityID,
		Type:        "local",
		Name:        "Local Admin",
		SyncEnabled: false,
	})
	if err != nil {
		t.Fatalf("create local identity source: %v", err)
	}
	user, err := queries.CreateManagedUser(ctx, generated.CreateManagedUserParams{
		EntityID:        entityID,
		Username:        "admin",
		DisplayName:     "Admin",
		Email:           pgtype.Text{String: "admin@example.test", Valid: true},
		LifecycleStatus: "active",
		UserType:        "employee",
		PrimarySourceID: pgtype.Text{String: source.ID, Valid: true},
		Locale:          pgtype.Text{String: "en-US", Valid: true},
	})
	if err != nil {
		t.Fatalf("create local user: %v", err)
	}
	return user
}

func createOIDCTestClient(ctx context.Context, t *testing.T, queries *generated.Queries, entityID string, clientID string, redirectURI string) generated.Application {
	t.Helper()
	app, err := queries.CreateApplication(ctx, generated.CreateApplicationParams{
		EntityID: entityID,
		Name:     "OIDC Console",
		Type:     "oidc_client",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	_, err = queries.CreateOIDCClient(ctx, generated.CreateOIDCClientParams{
		EntityID:           entityID,
		ApplicationID:      app.ID,
		ClientID:           clientID,
		ClientSecretHash:   pgtype.Text{String: clientsecret.Hash("secret-1"), Valid: true},
		RedirectUris:       []string{redirectURI},
		AllowedScopes:      []string{"openid", "profile", "email"},
		GrantTypes:         []string{"authorization_code"},
		ResponseTypes:      []string{"code"},
		PkceRequired:       true,
		WorkplaceProvider:  "",
		WorkplaceAppID:     "",
		WorkplaceAppSecret: "",
	})
	if err != nil {
		t.Fatalf("create oidc client: %v", err)
	}
	return app
}

func authorizeCode(ctx context.Context, t *testing.T, server *httptest.Server, entityID string, userID string, clientID string, redirectURI string, challenge string) string {
	t.Helper()
	res := authorizeRequest(ctx, t, server, entityID, userID, clientID, redirectURI, challenge, "ada@example.test", "Ada Lovelace")
	defer res.Body.Close()
	if res.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("authorize status = %d, want %d, body=%s", res.StatusCode, http.StatusFound, string(body))
	}
	location, err := url.Parse(res.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect location: %v", err)
	}
	if location.Query().Get("state") != "state-1" {
		t.Fatalf("state = %q, want state-1", location.Query().Get("state"))
	}
	code := location.Query().Get("code")
	if code == "" {
		t.Fatal("redirect code is empty")
	}
	return code
}

func authorizeRequest(ctx context.Context, t *testing.T, server *httptest.Server, entityID string, userID string, clientID string, redirectURI string, challenge string, username string, displayName string) *http.Response {
	t.Helper()
	values := url.Values{}
	values.Set("client_id", clientID)
	values.Set("redirect_uri", redirectURI)
	values.Set("response_type", "code")
	values.Set("scope", "openid email")
	values.Set("state", "state-1")
	values.Set("nonce", "nonce-1")
	values.Set("code_challenge", challenge)
	values.Set("code_challenge_method", "S256")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/oauth2/authorize?"+values.Encode(), nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	session, err := auth.EncodeSession(auth.Session{
		UserID:      pgULIDString(userID),
		EntityID:    pgULIDString(entityID),
		Username:    username,
		DisplayName: displayName,
	})
	if err != nil {
		t.Fatalf("encode session: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: "idb_session", Value: session})

	client := server.Client()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("authorize request: %v", err)
	}
	return res
}

func insertApplicationAssignment(ctx context.Context, t *testing.T, pool *pgxpool.Pool, entityID, applicationID, subjectType, subjectID, effect string) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO application_assignments (entity_id, application_id, subject_type, subject_id, effect)
		VALUES ($1, $2, $3, $4, $5)
	`, entityID, applicationID, subjectType, subjectID, effect)
	if err != nil {
		t.Fatalf("insert application assignment: %v", err)
	}
}

func getJSON(t *testing.T, endpoint string, wantStatus int) map[string]interface{} {
	t.Helper()
	res, err := http.Get(endpoint)
	if err != nil {
		t.Fatalf("GET %s: %v", endpoint, err)
	}
	defer res.Body.Close()
	if res.StatusCode != wantStatus {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("GET %s status = %d, want %d, body=%s", endpoint, res.StatusCode, wantStatus, string(body))
	}
	var payload map[string]interface{}
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	return payload
}

func postForm(t *testing.T, endpoint string, values url.Values, wantStatus int) map[string]interface{} {
	t.Helper()
	res, err := http.PostForm(endpoint, values)
	if err != nil {
		t.Fatalf("POST %s: %v", endpoint, err)
	}
	defer res.Body.Close()
	if res.StatusCode != wantStatus {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("POST %s status = %d, want %d, body=%s", endpoint, res.StatusCode, wantStatus, string(body))
	}
	var payload map[string]interface{}
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	return payload
}

func s256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func pgULIDString(value string) string {
	return value
}
