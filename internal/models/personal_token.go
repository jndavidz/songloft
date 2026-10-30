package models

import "time"

// PersonalToken 仅包含可公开的管理信息，不含凭证或哈希。
type PersonalToken struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

type CreatePersonalTokenRequest struct {
	Name string `json:"name" example:"Home Assistant"`
}

type CreatePersonalTokenResponse struct {
	PersonalToken
	Token string `json:"token"`
}

type PersonalTokenListResponse struct {
	Tokens []PersonalToken `json:"tokens"`
	Total  int64           `json:"total"`
	Limit  int             `json:"limit"`
	Offset int             `json:"offset"`
}
