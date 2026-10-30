package managed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// 准备操作状态机（S-03）：operation 绑定 issuer 与 session/epoch/entry，
// 迟到的 ready 不可投送。首版只覆盖准备状态与恢复语义；持久化 StateStore
// 接口先行（SQLite 实现待 sqlc 生成后接入）。

type OpState string

const (
	StatePreparing   OpState = "preparing"
	StateReady       OpState = "ready"
	StateFailed      OpState = "failed"
	StateCanceled    OpState = "canceled"
	StateInterrupted OpState = "interrupted" // 重启恢复：不自动续跑、不自动出声
)

// OpKey 把准备操作绑定到命令代次：session 内 epoch 变化即换代，
// 旧代次的完成结果一律视为迟到。
type OpKey struct {
	Session string
	Epoch   int64
	Entry   string
}

func (k OpKey) String() string { return fmt.Sprintf("%s/epoch%d/%s", k.Session, k.Epoch, k.Entry) }

// PrepareOp 是一条准备操作的快照。
type PrepareOp struct {
	Key       OpKey
	Issuer    string
	RefDigest string
	State     OpState
	Error     string
	CreatedAt time.Time
	UpdatedAt time.Time
	// Done 在终态（ready/failed/canceled/interrupted）后关闭。
	Done chan struct{}

	// ctx 驱动真实 worker（下载/转码）的生命周期：取消/换代不只是改状态，
	// 底层 goroutine 也必须真实退出，而不是等到下一个检查点。
	ctx    context.Context
	cancel context.CancelFunc

	// 解析事实（fallback 来源、降级原因）随 op 记录，供诊断与投送层使用。
	ResolvedSource string
	FallbackReason string
	// DurationSec 是验证得到的最终表示时长（MP3 帧估计）。
	// 投送层用它做「可信时长」EOF 判定，避免队列项缺时长导致永不推进。
	DurationSec float64
}

// PrepareDeps 注入准备链的外部能力；全部可 fake。
type PrepareDeps struct {
	Resolver ResolveAPI
	// Downloader 拉取完整媒体到本地（含字节/时间上限），返回实际落盘路径与大小。
	Downloader func(ctx context.Context, result *ResolveResult, destDir string) (string, int64, error)
	// Transcoder 把规范化输入转成 MP3（AAC 只经此路径，绝不直投）。
	Transcoder func(ctx context.Context, inputPath, destDir string) (string, error)
	// WorkDir 是表示隔离根：managed/<digest>/{source,intermediate,final}。
	WorkDir string
}

// Manager 维护准备操作表。
type Manager struct {
	deps PrepareDeps
	// store 可选持久化（SQLiteStateStore）；nil = 纯内存（测试）。
	store StateStore

	mu      sync.Mutex
	ops     map[string]*PrepareOp // key: issuer + "|" + OpKey.String()
	current map[string]string     // (session,entry) → 当前 op key（换代检测）
}

func NewManager(deps PrepareDeps) *Manager {
	return &Manager{
		deps:    deps,
		ops:     map[string]*PrepareOp{},
		current: map[string]string{},
	}
}

// WithStore 挂接持久化；必须在任何 StartPrepare 之前调用。
// Insert 在 StartPrepare 内同步执行（崩溃恢复的前提），Update 异步。
func (m *Manager) WithStore(s StateStore) *Manager {
	m.store = s
	return m
}

// RecoverOnRestart 把持久层里 state=preparing 的行显式转为 interrupted：
// 恢复元数据、绝不自动续跑、绝不自动出声（S-03 恢复语义）。返回行数。
func (m *Manager) RecoverOnRestart(ctx context.Context) (int, error) {
	if m.store == nil {
		return 0, nil
	}
	rows, err := m.store.ListActive(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, row := range rows {
		row.State = StateInterrupted
		row.UpdatedAt = time.Now()
		if err := m.store.Update(ctx, row); err == nil {
			n++
		}
	}
	return n, nil
}

func opID(issuer string, k OpKey) string {
	return issuer + "|" + k.String()
}

// startFinish 在持锁状态下把 op 推入终态；返回 false 表示 op 已在终态
// （Done 已关闭）——调用方绝不能在此路径上二次 close。
func (m *Manager) startFinish(op *PrepareOp, state OpState, errMsg string) bool {
	if op.State != StatePreparing {
		return false
	}
	op.State = state
	op.UpdatedAt = time.Now()
	if errMsg != "" {
		op.Error = errMsg
	}
	close(op.Done)
	op.cancel() // 真实 worker 退出：取消/失败/完成都不留跑着的下载
	m.persist(op)
	return true
}

// StartPrepare 为当前 entry 发起准备。同一 (session, entry) 只允许一个活跃代次：
// 新 epoch 到来时旧代次操作被取消并整体失效（迟到 ready 不可投送的来源）。
func (m *Manager) StartPrepare(issuer string, key OpKey, ref *Ref) (*PrepareOp, error) {
	if issuer == "" {
		return nil, errors.New("managed: prepare requires issuer identity")
	}
	if ref == nil {
		return nil, errors.New("managed: prepare requires track ref")
	}
	digest, err := ref.Digest()
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	curKey := key.Session + "|" + key.Entry
	if oldID, ok := m.current[curKey]; ok {
		old := m.ops[oldID]
		if old != nil && old.Key.Epoch != key.Epoch {
			// 换代：旧代次直接作废。startFinish 内部有终态守卫——
			// 旧 op 可能刚好在 fail/complete 与 defer 清理之间，不能二次 close。
			m.startFinish(old, StateCanceled, "")
			delete(m.ops, oldID)
		} else if old != nil {
			m.mu.Unlock()
			return nil, fmt.Errorf("managed: op already active for %s", curKey)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	op := &PrepareOp{
		Key: key, Issuer: issuer, RefDigest: digest,
		State: StatePreparing, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		Done: make(chan struct{}),
		ctx:  ctx, cancel: cancel,
	}
	id := opID(issuer, key)
	m.ops[id] = op
	m.current[curKey] = id
	m.mu.Unlock()

	if m.store != nil {
		insCtx, insCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := m.store.Insert(insCtx, op); err != nil {
			insCancel()
			m.mu.Lock()
			delete(m.ops, id)
			delete(m.current, curKey)
			m.mu.Unlock()
			op.cancel()
			close(op.Done)
			return nil, fmt.Errorf("managed: persist prepare op: %w", err)
		}
		insCancel()
	}

	go m.run(op, ref)
	return op, nil
}

// run 是准备管线：resolve → download → (transcode) → verify → ready。
// 全程绑定 op 自身 ctx：取消/换代让真实 worker 退出；结果投送前重验代次，
// 换代的完成一律丢弃。
func (m *Manager) run(op *PrepareOp, ref *Ref) {
	defer func() {
		op.cancel()
		m.mu.Lock()
		curKey := op.Key.Session + "|" + op.Key.Entry
		if m.current[curKey] == opID(op.Issuer, op.Key) {
			delete(m.current, curKey)
		}
		m.mu.Unlock()
	}()

	// 表示隔离根：managed/<digest>/，并发准备各写各的目录，绝不互踩。
	opDir := filepath.Join(m.deps.WorkDir, op.RefDigest)

	result, err := m.deps.Resolver.Resolve(op.ctx, ResolveRequest{
		Ref: ref, Target: "media_prepare", Freshness: "allow_cache",
	})
	if err != nil {
		m.fail(op, err)
		return
	}
	if result.URL == "" {
		m.fail(op, errors.New("resolver returned empty media url"))
		return
	}
	m.mu.Lock()
	op.ResolvedSource = result.ResolvedSource
	op.FallbackReason = result.FallbackReason
	m.mu.Unlock()
	if op.ctx.Err() != nil {
		return // resolve 期间已被取消/换代
	}

	srcPath, size, err := m.deps.Downloader(op.ctx, result, opDir)
	if err != nil {
		m.fail(op, err)
		return
	}

	// 解析结果声明了尺寸时，与实际下载字节数对照——「验证实际大小」的契约环。
	// 但转码输入不是最终投送表示：bilibili 等源声明的 size 与实际 DASH 流字节
	// 常有偏差（实测 6,201,843 vs 6,469,840），此时以**实际字节**为准并留告警；
	// 直投 MP3 仍保持严格相等（字节就是最终表示）。
	if result.SizeBytes > 0 && size != result.SizeBytes {
		if !result.Verification.RequiresTranscode {
			m.fail(op, fmt.Errorf("declared size %d != downloaded %d bytes", result.SizeBytes, size))
			return
		}
		slog.Warn("managed: declared size differs for transcode input (actual wins)",
			"declared", result.SizeBytes, "actual", size, "source", result.ResolvedSource)
	}

	finalPath := srcPath
	if result.Verification.RequiresTranscode {
		finalPath, err = m.deps.Transcoder(op.ctx, srcPath, opDir)
		if err != nil {
			m.fail(op, err)
			return
		}
	}

	expect := VerifyExpectation{MinDurationSec: 1, MaxBitrateKbps: 320}
	// 声明的大小只对「直投 MP3」有意义：转码后的 final 是另一份字节，
	// 拿输入声明大小去校验输出必然不符（bilibili 等源的真实情况）。
	if result.SizeBytes > 0 && !result.Verification.RequiresTranscode {
		expect.DeclaredSize = size
	}
	rep, err := VerifyMP3(finalPath, expect)
	if err != nil {
		m.fail(op, err)
		return
	}
	m.mu.Lock()
	op.DurationSec = rep.DurationEstSec
	m.mu.Unlock()

	// 验证通过的表示落位为最终名（票据指向 final.mp3）：
	// 拷贝而非改名——原始 source.* 与中间 intermediate.mp3 保留（表示隔离，
	// 原码不丢：失败重转码/后续 GC 判定都需要）。
	finalName := filepath.Join(opDir, "final.mp3")
	if finalPath != finalName {
		if err := copyFile(finalPath, finalName); err != nil {
			m.fail(op, fmt.Errorf("promote final representation: %w", err))
			return
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.startFinish(op, StateReady, "")
}

func (m *Manager) fail(op *PrepareOp, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.startFinish(op, StateFailed, err.Error())
}

// Cancel 撤销操作；issuer 不匹配拒绝（跨主体不得互取消）。
func (m *Manager) Cancel(issuer string, key OpKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	op, ok := m.ops[opID(issuer, key)]
	if !ok {
		return fmt.Errorf("managed: no op for %s", key)
	}
	if op.Issuer != issuer {
		return fmt.Errorf("managed: issuer mismatch, cancel denied")
	}
	m.startFinish(op, StateCanceled, "")
	return nil
}

// Query 返回操作快照；issuer 不匹配视同不存在（不泄露存在性）。
func (m *Manager) Query(issuer string, key OpKey) (*PrepareOp, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	op, ok := m.ops[opID(issuer, key)]
	if !ok || op.Issuer != issuer {
		return nil, false
	}
	cp := *op
	return &cp, true
}

// MarkAllInterruptedOnRestart 是恢复语义的显式入口：进程重启后所有未完成
// 操作进入 interrupted——恢复元数据但绝不自动续跑、绝不自动出声。
func (m *Manager) MarkAllInterruptedOnRestart() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, op := range m.ops {
		m.startFinish(op, StateInterrupted, "")
	}
}

// ActiveCount 诊断用：仍在 preparing 的操作数。
func (m *Manager) ActiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, op := range m.ops {
		if op.State == StatePreparing {
			n++
		}
	}
	return n
}

// copyFile 流式拷贝（表示落位用，不整载入内存）。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

// copyFile 流式拷贝（表示落位用，不整载入内存）。
