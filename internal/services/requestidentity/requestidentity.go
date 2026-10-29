// Package requestidentity 统一表示「这次请求／这项系统工作以谁的身份在跑」。
//
// middleware 是唯一写入方：认证成功后把 Identity 放进请求 ctx；各服务从 ctx
// 统一读取（playactivity 会话分桶等）。此前 playactivity 用裸 string key 自行读
// client_id，与 middleware 的类型化私有 key 类型不同永远取不到值，所有认证请求
// 都落进共享零值桶、跨客户端互取消（S-01）。
//
// 依赖方向：middleware 与各服务都只 import 本包，服务层不得反向 import middleware。
package requestidentity

import (
	"context"
	"errors"
	"strings"
)

// Kind 区分身份类别。
type Kind string

const (
	KindUser   Kind = "user"   // 真实认证主体（JWT client_id）
	KindSystem Kind = "system" // 显式系统工作（后台任务、内部调用）
)

// Identity 是一次请求或一项系统工作的身份。
//
// 零值 Identity 不是合法身份：缺身份的 ctx 不携带任何 Identity，消费方
// （如 playactivity.SessionFromContext）必须自行给出互不共享的兜底桶，
// 不得把「没有身份」当成可共享的成员。
type Identity struct {
	Kind     Kind
	ClientID string // KindUser 时为认证主体；KindSystem 时恒为空
}

// System 返回显式系统身份。系统工作必须用它，不得伪装成用户身份或零值。
func System() Identity { return Identity{Kind: KindSystem} }

// User 构造认证用户身份。clientID 为空返回错误：真实认证请求缺主体必须在
// 写入 ctx 之前失败关闭，不得落入共享零值桶。
func User(clientID string) (Identity, error) {
	if strings.TrimSpace(clientID) == "" {
		return Identity{}, errors.New("requestidentity: 用户身份缺少 client_id")
	}
	return Identity{Kind: KindUser, ClientID: clientID}, nil
}

// String 输出可诊断的身份串："user:<client_id>" / "system"。
func (i Identity) String() string {
	if i.Kind == KindUser {
		return "user:" + i.ClientID
	}
	return string(i.Kind)
}

// IsZero 报告该 Identity 是否为未构造的零值。
func (i Identity) IsZero() bool { return i == Identity{} }

type ctxKey struct{}

// WithIdentity 把身份写入 ctx。仅供 middleware 与显式声明系统工作的构造方使用；
// 服务层只读（FromContext），不得另写身份。
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// SystemContext 返回携带显式系统身份的 ctx，供后台任务／内部调用使用。
func SystemContext(ctx context.Context) context.Context {
	return WithIdentity(ctx, System())
}

// FromContext 读出身份；ok=false 表示 ctx 未携带身份（未走鉴权中间件、或系统
// 工作未显式声明身份）。消费方对 ok=false 的请求不得落入任何共享桶。
func FromContext(ctx context.Context) (Identity, bool) {
	if ctx == nil {
		return Identity{}, false
	}
	id, ok := ctx.Value(ctxKey{}).(Identity)
	if !ok || id.IsZero() {
		return Identity{}, false
	}
	return id, true
}
