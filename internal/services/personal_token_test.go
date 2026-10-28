package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"songloft/internal/database"
	"songloft/internal/database/testutil"
)

func TestPersonalTokenLifecycle(t *testing.T) {
	ctx := context.Background()
	db := testutil.OpenMemoryDB(t)
	newService := func() *AuthService {
		svc, err := NewAuthService(db.ConfigRepository(), db.TokenRepository(), "admin", "password")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(svc.Close)
		return svc
	}
	svc := newService()
	first, err := svc.CreatePersonalToken(ctx, " Home Assistant ")
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.CreatePersonalToken(ctx, "中控屏")
	if err != nil {
		t.Fatal(err)
	}
	if first.Name != "Home Assistant" || first.Token == second.Token || first.ID == second.ID || len(first.Token) != 50 {
		t.Fatal("invalid generated tokens")
	}
	var stored string
	if err := db.DB().QueryRowContext(ctx, "SELECT token_hash FROM personal_tokens WHERE id = ?", first.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != personalTokenHash(first.Token) || strings.Contains(stored, first.Token) {
		t.Fatal("credential was not hashed")
	}
	if _, err := svc.ValidateToken(ctx, first.Token+"x"); err == nil {
		t.Fatal("accepted malformed token")
	}
	if _, err := svc.ValidateToken(ctx, second.Token[:49]+"!"); err == nil {
		t.Fatal("accepted unknown token")
	}
	claims, err := svc.ValidateToken(ctx, first.Token)
	if err != nil || claims.ClientID != "personal-token:"+first.ID || claims.CredentialType != "personal" || claims.ExpiresAt != nil {
		t.Fatalf("unexpected principal: %v %v", claims, err)
	}
	page, err := svc.ListPersonalTokens(ctx, 1, 0)
	if err != nil || len(page.Tokens) != 1 || page.Total != 2 {
		t.Fatalf("pagination: %v %v", page, err)
	}
	all, _ := svc.ListPersonalTokens(ctx, 20, 0)
	raw, _ := json.Marshal(all)
	if strings.Contains(string(raw), first.Token) || strings.Contains(string(raw), stored) {
		t.Fatal("list exposes secret")
	}
	before, err := db.TokenRepository().GetPersonalTokenByHash(ctx, stored)
	if err != nil || before.LastUsedAt == nil {
		t.Fatal("last use not recorded")
	}
	if _, err := svc.ValidateToken(ctx, first.Token); err != nil {
		t.Fatal(err)
	}
	after, _ := db.TokenRepository().GetPersonalTokenByHash(ctx, stored)
	if !before.LastUsedAt.Equal(*after.LastUsedAt) {
		t.Fatal("last use updated more than once per minute")
	}
	// Another service instance has no session cache; the persisted token still works.
	restarted := newService()
	if _, err := restarted.ValidateToken(ctx, first.Token); err != nil {
		t.Fatal(err)
	}
	if err := svc.RevokePersonalToken(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.RevokePersonalToken(ctx, first.ID); err != nil {
		t.Fatal("repeated revoke failed", err)
	}
	if _, err := svc.ValidateToken(ctx, first.Token); err == nil {
		t.Fatal("revoked token accepted by original instance")
	}
	if _, err := restarted.ValidateToken(ctx, first.Token); err == nil {
		t.Fatal("revoked token accepted by other instance")
	}
	if _, err := restarted.ValidateToken(ctx, second.Token); err != nil {
		t.Fatal("another device was revoked", err)
	}
	if err := svc.RevokePersonalToken(ctx, "missing"); !errors.Is(err, database.ErrNotFound) {
		t.Fatal("missing token semantics", err)
	}
	if _, err := db.DB().ExecContext(ctx, "UPDATE personal_tokens SET last_used_at = ? WHERE id = ?", time.Now().UTC().Add(-2*time.Minute), second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ValidateToken(ctx, second.Token); err != nil {
		t.Fatal(err)
	}
	updated, _ := db.TokenRepository().GetPersonalTokenByHash(ctx, personalTokenHash(second.Token))
	if updated.LastUsedAt == nil || time.Since(*updated.LastUsedAt) > time.Second {
		t.Fatal("stale last use not refreshed")
	}
}

func TestPersonalTokenNameValidation(t *testing.T) {
	env := newAuthTestEnv(t)
	svc, err := NewAuthService(env.configs, env.tokens, "admin", "password")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	for _, name := range []string{"", "  ", "a\nb", strings.Repeat("中", 81), string([]byte{0xff})} {
		if _, err := svc.CreatePersonalToken(context.Background(), name); !errors.Is(err, ErrPersonalTokenName) {
			t.Fatalf("accepted invalid name: %q", name)
		}
	}
	if _, err := svc.CreatePersonalToken(context.Background(), strings.Repeat("中", 80)); err != nil {
		t.Fatal(err)
	}
}
