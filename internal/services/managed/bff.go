package managed

import (
	"context"
	"fmt"
	"sync"
)

// S-06：托管 BFF 会话状态机。
//
// - bridge 始终是 LX06 队列权威；手机只发 TrackRef/命令，不建立第二 managed 队列；
// - 手机从原生 DLNA 转 managed 前必须先结束旧续播职责：handoff 分两段
//   （请求接管 → 显式 Stop 确认 → managed 生效），接管不双播；
// - 旧 epoch 命令一律拒绝；换代后旧 App 不自动恢复 owner；
// - 本机播放不注册到 bridge：只有显式 handoff 才改变阶段，本地播放路径
//   与本状态机零接触（构造性保证）。

// BFFPhase 会话阶段：native（手机原生续播）→ handoff-pending（等 Stop 确认）
// → managed（bridge 托管生效）。
type BFFPhase string

const (
	PhaseNative         BFFPhase = "native"
	PhaseHandoffPending BFFPhase = "handoff-pending"
	PhaseManaged        BFFPhase = "managed"
)

// QueueEntry 队列项：entry 唯一标识一次出现（同曲可多次入队），身份是 TrackRef digest。
type QueueEntry struct {
	Entry     string
	RefDigest string
}

// Command 手机/BFF 发来的托管命令。
type Command struct {
	Name  string // play | pause | stop | next | jump
	Epoch int64
	Entry string // jump 的目标
}

// BFFSession 单会话状态。
type BFFSession struct {
	Session string
	Epoch   int64
	Phase   BFFPhase
	Queue   []QueueEntry
	Current int
}

// ManagedBFF 维护会话表。Tickets 可选注入（换代时撤销旧代次票据）。
type ManagedBFF struct {
	mu      sync.Mutex
	sess    map[string]*BFFSession
	tickets *TicketIssuer
}

func NewManagedBFF(tickets *TicketIssuer) *ManagedBFF {
	return &ManagedBFF{sess: map[string]*BFFSession{}, tickets: tickets}
}

func (b *ManagedBFF) session(session string) *BFFSession {
	s, ok := b.sess[session]
	if !ok {
		s = &BFFSession{Session: session, Epoch: 1, Phase: PhaseNative}
		b.sess[session] = s
	}
	return s
}

// State 返回会话快照。
func (b *ManagedBFF) State(session string) BFFSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := *b.session(session)
	return cp
}

// SetQueue 手机提交整队（TrackRef digest 列表）。仅 TrackRef 身份，无 URL。
// 队列编辑在任何阶段都允许（编辑≠建立第二播放队列）。
func (b *ManagedBFF) SetQueue(session string, epoch int64, entries []QueueEntry) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.session(session)
	if epoch != s.Epoch {
		return fmt.Errorf("bff: stale epoch %d (current %d)", epoch, s.Epoch)
	}
	s.Queue = append([]QueueEntry(nil), entries...)
	s.Current = 0
	return nil
}

// RequestHandoff 第一步：手机声明交出原生续播职责。返回待确认阶段；
// 此刻 bridge 还不能开始播放（防双播）。
func (b *ManagedBFF) RequestHandoff(session string) (BFFPhase, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.session(session)
	switch s.Phase {
	case PhaseManaged:
		return s.Phase, nil // 幂等：已托管
	case PhaseNative:
		s.Phase = PhaseHandoffPending
		return s.Phase, nil
	default:
		return s.Phase, nil
	}
}

// ConfirmHandoffStop 第二步：旧续播职责已显式 Stop 并确认（无第二 owner）
// 之后，managed 生效。
func (b *ManagedBFF) ConfirmHandoffStop(session string, epoch int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.session(session)
	if epoch != s.Epoch {
		return fmt.Errorf("bff: stale epoch %d (current %d)", epoch, s.Epoch)
	}
	if s.Phase != PhaseHandoffPending {
		return fmt.Errorf("bff: handoff not pending (phase=%s)", s.Phase)
	}
	s.Phase = PhaseManaged
	return nil
}

// Execute 执行命令。旧 epoch/非 managed 阶段的播放命令一律拒绝；
// stop 在任何阶段都接受（stop 优先），且换代时撤销该 entry 票据。
func (b *ManagedBFF) Execute(ctx context.Context, session string, cmd Command) error {
	b.mu.Lock()
	s := b.session(session)
	if cmd.Epoch != s.Epoch {
		epoch := s.Epoch
		b.mu.Unlock()
		return fmt.Errorf("bff: stale epoch %d (current %d)", cmd.Epoch, epoch)
	}
	switch cmd.Name {
	case "stop":
		// stop 先于一切：接受并撤销当前代次该 entry 的在途票据
		if b.tickets != nil {
			_ = b.tickets.RevokeEntry(ctx, session, s.Epoch, cmd.Entry)
		}
		b.mu.Unlock()
		return nil
	case "play", "next", "jump":
		if s.Phase != PhaseManaged {
			phase := s.Phase
			b.mu.Unlock()
			return fmt.Errorf("bff: playback command requires managed phase (now %s)", phase)
		}
		if cmd.Name == "jump" || cmd.Name == "play" {
			for i, e := range s.Queue {
				if e.Entry == cmd.Entry {
					s.Current = i
					break
				}
			}
		} else if cmd.Name == "next" && s.Current < len(s.Queue)-1 {
			s.Current++
		}
		b.mu.Unlock()
		return nil
	case "pause":
		b.mu.Unlock()
		return nil
	default:
		b.mu.Unlock()
		return fmt.Errorf("bff: unknown command %q", cmd.Name)
	}
}

// NewEpoch 换代：旧代次全部命令/票据失效（旧 App 不自动恢复 owner）。
func (b *ManagedBFF) NewEpoch(session string) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.session(session)
	s.Epoch++
	// 旧代次票据由调用方按需批量撤销；状态机只推进代次。
	return s.Epoch
}

// AlbumItemResult 专辑逐项映射结果：缺身份的项保留显式错误，绝不静默错配。
type AlbumItemResult struct {
	Index int    `json:"index"`
	Title string `json:"title,omitempty"`
	Ref   *Ref   `json:"track_ref,omitempty"`
	Error string `json:"error,omitempty"`
}

// MapAlbum 把插件的专辑项列表（source_data 数组）映射为 TrackRef。
// 单项失败不吞整专辑：失败项带 index+错误原样返回（可见、可重试、可跳过），
// 成功项保留完整身份（B站 cid/page、咪咕 copyrightId 经 RefFromSourceData 契约）。
// 全部失败时返回错误（整专辑不可用要有提示，但不是静默消失）。
func MapAlbum(sourceDatas []string, titles []string, sources []string) ([]AlbumItemResult, error) {
	out := make([]AlbumItemResult, len(sourceDatas))
	failed := 0
	for i, sd := range sourceDatas {
		res := AlbumItemResult{Index: i}
		if i < len(titles) {
			res.Title = titles[i]
		}
		ref, err := RefFromSourceData(sd, sources)
		if err != nil {
			res.Error = err.Error()
			failed++
		} else {
			res.Ref = ref
		}
		out[i] = res
	}
	if failed == len(sourceDatas) {
		return out, fmt.Errorf("managed: all %d album items failed identity mapping (album unusable, surface the error)", failed)
	}
	return out, nil
}
