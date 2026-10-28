package database

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"songloft/internal/database/sqlc"
	"songloft/internal/models"
)

// 永久令牌与登录令牌共用认证仓储入口，但使用独立表和哈希索引。
func (r *TokenRepository) CreatePersonalToken(ctx context.Context, token models.PersonalToken, hash string) error {
	return r.queries.CreatePersonalToken(ctx, sqlc.CreatePersonalTokenParams{
		ID: token.ID, Name: token.Name, TokenHash: hash, Prefix: token.Prefix, CreatedAt: token.CreatedAt,
	})
}

func (r *TokenRepository) GetPersonalTokenByHash(ctx context.Context, hash string) (*models.PersonalToken, error) {
	row, err := r.queries.GetPersonalTokenByHash(ctx, hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	token := personalTokenModel(row.ID, row.Name, row.Prefix, row.CreatedAt, row.LastUsedAt, row.RevokedAt)
	return &token, nil
}

func (r *TokenRepository) ListPersonalTokens(ctx context.Context, limit, offset int) ([]models.PersonalToken, int64, error) {
	rows, err := r.queries.ListPersonalTokens(ctx, sqlc.ListPersonalTokensParams{Limit: int64(limit), Offset: int64(offset)})
	if err != nil {
		return nil, 0, err
	}
	total, err := r.queries.CountPersonalTokens(ctx)
	if err != nil {
		return nil, 0, err
	}
	tokens := make([]models.PersonalToken, 0, len(rows))
	for _, row := range rows {
		tokens = append(tokens, personalTokenModel(row.ID, row.Name, row.Prefix, row.CreatedAt, row.LastUsedAt, row.RevokedAt))
	}
	return tokens, total, nil
}

func (r *TokenRepository) RevokePersonalToken(ctx context.Context, id string) error {
	n, err := r.queries.RevokePersonalToken(ctx, sqlc.RevokePersonalTokenParams{ID: id, RevokedAt: sql.NullTime{Time: time.Now().UTC(), Valid: true}})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *TokenRepository) TouchPersonalToken(ctx context.Context, id string, now time.Time) error {
	return r.queries.TouchPersonalToken(ctx, sqlc.TouchPersonalTokenParams{
		ID: id, LastUsedAt: sql.NullTime{Time: now, Valid: true}, LastUsedAt_2: sql.NullTime{Time: now.Add(-time.Minute), Valid: true},
	})
}

func personalTokenModel(id, name, prefix string, created time.Time, used, revoked sql.NullTime) models.PersonalToken {
	token := models.PersonalToken{ID: id, Name: name, Prefix: prefix, CreatedAt: created}
	if used.Valid {
		token.LastUsedAt = &used.Time
	}
	if revoked.Valid {
		token.RevokedAt = &revoked.Time
	}
	return token
}
