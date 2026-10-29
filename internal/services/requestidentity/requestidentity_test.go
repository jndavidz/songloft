package requestidentity

import (
	"context"
	"testing"
)

func TestUser(t *testing.T) {
	t.Run("正常构造", func(t *testing.T) {
		id, err := User("client-A")
		if err != nil {
			t.Fatalf("User(client-A) 不应报错: %v", err)
		}
		if id.Kind != KindUser || id.ClientID != "client-A" {
			t.Fatalf("got %+v", id)
		}
		if id.String() != "user:client-A" {
			t.Fatalf("String()=%q", id.String())
		}
	})
	t.Run("缺主体失败关闭", func(t *testing.T) {
		for _, empty := range []string{"", "   "} {
			if _, err := User(empty); err == nil {
				t.Errorf("User(%q) 应报错，不得产出可落入共享桶的身份", empty)
			}
		}
	})
}

func TestSystemAndZero(t *testing.T) {
	sys := System()
	if sys.Kind != KindSystem || sys.ClientID != "" {
		t.Fatalf("System()=%+v", sys)
	}
	if sys.String() != "system" {
		t.Fatalf("String()=%q", sys.String())
	}
	var zero Identity
	if !zero.IsZero() {
		t.Error("零值 Identity 应 IsZero()")
	}
	if sys.IsZero() {
		t.Error("System() 不应 IsZero()")
	}
}

func TestContextRoundTrip(t *testing.T) {
	t.Run("user 身份存取", func(t *testing.T) {
		id, _ := User("client-B")
		ctx := WithIdentity(context.Background(), id)
		got, ok := FromContext(ctx)
		if !ok || got != id {
			t.Fatalf("got %+v ok=%v", got, ok)
		}
	})
	t.Run("system 身份存取", func(t *testing.T) {
		ctx := SystemContext(context.Background())
		got, ok := FromContext(ctx)
		if !ok || got.Kind != KindSystem {
			t.Fatalf("got %+v ok=%v", got, ok)
		}
	})
	t.Run("无身份返回 ok=false", func(t *testing.T) {
		for _, ctx := range []context.Context{nil, context.Background()} {
			if _, ok := FromContext(ctx); ok {
				t.Errorf("ctx %v 不应携带身份", ctx)
			}
		}
	})
	t.Run("写入零值身份视同无身份", func(t *testing.T) {
		ctx := WithIdentity(context.Background(), Identity{})
		if _, ok := FromContext(ctx); ok {
			t.Error("零值 Identity 不得被当成合法身份读出")
		}
	})
}
