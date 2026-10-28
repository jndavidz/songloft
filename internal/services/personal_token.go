package services

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"songloft/internal/models"
)

var ErrPersonalTokenName = errors.New("名称须为 1 至 80 个字符，且不能包含控制字符")

const PersonalTokenPrefix = "sl_pat_"

func (s *AuthService) CreatePersonalToken(ctx context.Context, name string) (*models.CreatePersonalTokenResponse, error) {
	name = strings.TrimSpace(name)
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) < 1 || utf8.RuneCountInString(name) > 80 || strings.ContainsFunc(name, unicode.IsControl) {
		return nil, ErrPersonalTokenName
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	raw := PersonalTokenPrefix + base64.RawURLEncoding.EncodeToString(secret)
	token := models.PersonalToken{ID: hex.EncodeToString(id), Name: name, Prefix: raw[:15], CreatedAt: time.Now().UTC()}
	if err := s.tokens.CreatePersonalToken(ctx, token, personalTokenHash(raw)); err != nil {
		return nil, err
	}
	return &models.CreatePersonalTokenResponse{PersonalToken: token, Token: raw}, nil
}

func personalTokenHash(raw string) string {
	hash := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(hash[:])
}

func (s *AuthService) validatePersonalToken(ctx context.Context, raw string) (*Claims, error) {
	if len(raw) != len(PersonalTokenPrefix)+43 {
		return nil, fmt.Errorf("invalid personal token")
	}
	token, err := s.tokens.GetPersonalTokenByHash(ctx, personalTokenHash(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid personal token: %w", err)
	}
	if token.RevokedAt != nil {
		return nil, fmt.Errorf("personal token has been revoked")
	}
	now := time.Now().UTC()
	if token.LastUsedAt == nil || now.Sub(*token.LastUsedAt) >= time.Minute {
		if err := s.tokens.TouchPersonalToken(ctx, token.ID, now); err != nil {
			return nil, err
		}
	}
	return &Claims{ClientID: "personal-token:" + token.ID, CredentialType: "personal"}, nil
}

func (s *AuthService) ListPersonalTokens(ctx context.Context, limit, offset int) (*models.PersonalTokenListResponse, error) {
	tokens, total, err := s.tokens.ListPersonalTokens(ctx, limit, offset)
	if err != nil {
		return nil, err
	}
	return &models.PersonalTokenListResponse{Tokens: tokens, Total: total, Limit: limit, Offset: offset}, nil
}

func (s *AuthService) RevokePersonalToken(ctx context.Context, id string) error {
	return s.tokens.RevokePersonalToken(ctx, id)
}
