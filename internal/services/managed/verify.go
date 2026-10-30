package managed

import (
	"bytes"
	"fmt"
	"os"
)

// 最终表示验证（S-03）：只有完整验证通过的 ≤320k MP3 才能进入 ready。
// URL 后缀、MIME、HEAD、部分嗅探、布尔 verified 都不算验证——本文件对
// 最终字节做帧级检查：ID3v2 跳过、逐帧同步/帧长推进、每帧码率上限、
// 时长估计，ffprobe 交叉验证经 Prober 注入（可选）。

// mp3BitrateTable[kbps]，单位 kbps；索引 0 与 15 非法。MPEG1 Layer III 与
// MPEG2/2.5 Layer III 各一张（契约值见 ISO 11172-3 / 13818-3）。
var (
	mp3BitrateMPEG1   = [16]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}
	mp3BitrateMPEG2   = [16]int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0}
	mp3SampleMPEG1    = [4]int{44100, 48000, 32000, 0}
	mp3SampleMPEG2    = [4]int{22050, 24000, 16000, 0}
	mp3SampleMPEG25   = [4]int{11025, 12000, 8000, 0}
	mp3SamplesPerMPEG = map[byte]int{3: 1152, 2: 576, 0: 576} // version bits → samples/frame (Layer III)
)

// Prober 用外部工具（ffprobe）交叉验证容器事实；nil 表示跳过交叉验证。
// codec 为空或与 "mp3" 不符即验证失败。
type Prober func(path string) (codec string, durationSec float64, err error)

// VerifyExpectation 声明验证期望。零值字段表示不检查该项。
type VerifyExpectation struct {
	MinDurationSec float64 // ffprobe/估计时长下限
	MaxBitrateKbps int     // 每帧码率上限（托管契约 320）
	DeclaredSize   int64   // 解析结果声明的字节数（>0 时要求一致）
	MaxDurationSec float64 // 时长上限（可选）
	Prober         Prober  // 可选
	ToleranceSec   float64 // ffprobe 与帧估计时长容差，默认 1.5s
}

// VerifyReport 汇总一次完整验证的实测事实。
type VerifyReport struct {
	SizeBytes         int64   `json:"size_bytes"`
	FrameCount        int     `json:"frame_count"`
	ID3PrefixBytes    int     `json:"id3_prefix_bytes"`
	DurationEstSec    float64 `json:"duration_est_sec"`
	ProbeCodec        string  `json:"probe_codec,omitempty"`
	ProbeDurationSec  float64 `json:"probe_duration_sec,omitempty"`
	MaxFrameKbps      int     `json:"max_frame_kbps"`
	AvgKbps           int     `json:"avg_kbps"`
	TrailingJunkBytes int     `json:"trailing_junk_bytes"`
}

// VerifyMP3 对最终表示做完整验证；任何一项不满足都返回 error，
// 绝不降级为「原样投送」。
func VerifyMP3(path string, exp VerifyExpectation) (*VerifyReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read final representation: %w", err)
	}
	rep := &VerifyReport{SizeBytes: int64(len(data))}

	if exp.DeclaredSize > 0 && rep.SizeBytes != exp.DeclaredSize {
		return nil, fmt.Errorf("size mismatch: declared %d, actual %d", exp.DeclaredSize, rep.SizeBytes)
	}

	off, err := skipID3v2(data)
	if err != nil {
		return nil, err
	}
	rep.ID3PrefixBytes = off
	if off >= len(data) {
		return nil, fmt.Errorf("no audio frames after ID3 tag")
	}

	maxKbps := exp.MaxBitrateKbps
	if maxKbps <= 0 {
		maxKbps = 320
	}
	var totalSamples int64
	var sampleRate int
	gapBytes := 0
	maxTotalGap := len(data) / 100 // 重同步预算：文件大小的 1%
	if maxTotalGap < 4*1024 {
		maxTotalGap = 4 * 1024
	}
	scan := off
	for scan+4 <= len(data) {
		if !frameSyncAt(data, scan) {
			// 非帧头：真实世界文件存在厂商填充/垃圾（如 kuwo 尾部 0x55 块）。
			// 有界重同步：窗口内找「连续两帧都合法」的位置继续；预算超限即拒。
			next, ok := resync(data, scan, sampleRate, maxKbps)
			if !ok {
				// 无法重同步 → 视作尾部：残余必须在预算内且含标签标记
				// （截断的帧碎片不含标记，照旧拒绝——截断防线不放松）
				tail := data[scan:]
				if len(tail) <= 128*1024 && containsTagMarker(tail) {
					rep.TrailingJunkBytes = len(tail)
					break
				}
				return nil, fmt.Errorf("expected frame sync at offset %d (after %d audio bytes): not a recognizable frame stream or tagged tail", scan, scan-off)
			}
			gapBytes += next - scan
			if gapBytes > maxTotalGap {
				return nil, fmt.Errorf("resync gap budget exceeded: %d bytes of junk (limit %d)", gapBytes, maxTotalGap)
			}
			scan = next
			continue
		}
		version := (data[scan+1] >> 3) & 0x03 // 3=MPEG1 2=MPEG2 0=MPEG2.5 1=reserved
		layer := (data[scan+1] >> 1) & 0x03   // 1=Layer III
		if version == 0x01 || layer != 0x01 {
			return nil, fmt.Errorf("frame at offset %d: not MPEG Layer III (version bits %d, layer bits %d)", scan, version, layer)
		}
		b2 := data[scan+2]
		brIdx := int((b2 >> 4) & 0x0F)
		srIdx := int((b2 >> 2) & 0x03)
		padding := int((b2 >> 1) & 0x01)
		var bitrate [16]int
		var sampleTbl [4]int
		switch version {
		case 3:
			copy(bitrate[:], mp3BitrateMPEG1[:])
			sampleTbl = mp3SampleMPEG1
		case 2:
			copy(bitrate[:], mp3BitrateMPEG2[:])
			sampleTbl = mp3SampleMPEG2
		case 0:
			copy(bitrate[:], mp3BitrateMPEG2[:])
			sampleTbl = mp3SampleMPEG25
		}
		if brIdx == 0 || brIdx == 15 || bitrate[brIdx] == 0 {
			return nil, fmt.Errorf("frame at offset %d: invalid bitrate index %d", scan, brIdx)
		}
		if srIdx == 3 || sampleTbl[srIdx] == 0 {
			return nil, fmt.Errorf("frame at offset %d: invalid sample rate index %d", scan, srIdx)
		}
		kbps := bitrate[brIdx]
		if kbps > maxKbps {
			return nil, fmt.Errorf("frame at offset %d: %dkbps exceeds %dkbps ceiling", scan, kbps, maxKbps)
		}
		sr := sampleTbl[srIdx]
		if sampleRate == 0 {
			sampleRate = sr
		} else if sampleRate != sr {
			return nil, fmt.Errorf("frame at offset %d: sample rate changed %d→%d mid-stream", scan, sampleRate, sr)
		}
		spf := mp3SamplesPerMPEG[version]
		frameLen := (spf/8)*kbps*1000/sr + padding // L3: 144 或 72 = spf/8
		if frameLen <= 0 {
			return nil, fmt.Errorf("frame at offset %d: non-positive frame length", scan)
		}
		rep.FrameCount++
		totalSamples += int64(spf)
		if kbps > rep.MaxFrameKbps {
			rep.MaxFrameKbps = kbps
		}
		scan += frameLen
	}
	if scan != len(data) && rep.TrailingJunkBytes == 0 {
		// 循环自然退出（残余 <4 字节）：残余必须含标签标记，截断防线不放松。
		tail := data[scan:]
		if !isRecognizedTailTag(tail) {
			return nil, fmt.Errorf("%d trailing bytes after last frame at %d are not a recognized tag", len(tail), scan)
		}
		rep.TrailingJunkBytes = len(tail)
	}
	if rep.FrameCount == 0 {
		return nil, fmt.Errorf("zero MP3 frames found")
	}
	rep.DurationEstSec = float64(totalSamples) / float64(sampleRate)
	if rep.SizeBytes > 0 {
		rep.AvgKbps = int(float64(rep.SizeBytes*8) / rep.DurationEstSec / 1000)
	}

	if exp.MinDurationSec > 0 && rep.DurationEstSec < exp.MinDurationSec {
		return nil, fmt.Errorf("duration %.2fs below minimum %.2fs", rep.DurationEstSec, exp.MinDurationSec)
	}
	if exp.MaxDurationSec > 0 && rep.DurationEstSec > exp.MaxDurationSec {
		return nil, fmt.Errorf("duration %.2fs above maximum %.2fs", rep.DurationEstSec, exp.MaxDurationSec)
	}
	if exp.Prober != nil {
		codec, dur, err := exp.Prober(path)
		if err != nil {
			return nil, fmt.Errorf("probe failed: %w", err)
		}
		rep.ProbeCodec = codec
		rep.ProbeDurationSec = dur
		if codec != "mp3" {
			return nil, fmt.Errorf("probe codec %q is not mp3", codec)
		}
		tol := exp.ToleranceSec
		if tol <= 0 {
			tol = 1.5
		}
		if dur > 0 && abs64(dur-rep.DurationEstSec) > tol {
			return nil, fmt.Errorf("probe duration %.2fs diverges from frame estimate %.2fs beyond %.2fs", dur, rep.DurationEstSec, tol)
		}
	}
	return rep, nil
}

// skipID3v2 跳过文件头的 ID3v2 标签；无标签返回 0。
func skipID3v2(data []byte) (int, error) {
	if len(data) < 10 || !bytes.Equal(data[:3], []byte("ID3")) {
		return 0, nil
	}
	flags := data[5]
	size := int64(0)
	for _, b := range data[6:10] {
		size = size<<7 | int64(b&0x7F) // syncsafe
	}
	total := 10 + size
	if flags&0x10 != 0 { // footer present
		total += 10
	}
	if total <= 0 || total > int64(len(data)) {
		return 0, fmt.Errorf("ID3v2 tag size %d exceeds file size %d", total, len(data))
	}
	return int(total), nil
}

// isRecognizedTailTag 报告末尾残余是否是已知标签：ID3v1（TAG+128）、
// APEv2（APETAGEX 前缀）、Lyrics3v2（LYRICS200）。其余一律不是。
func isRecognizedTailTag(tail []byte) bool {
	if len(tail) == 0 {
		return true
	}
	if len(tail) >= 3 && string(tail[:3]) == "TAG" {
		return true
	}
	if len(tail) >= 8 && string(tail[:8]) == "APETAGEX" {
		return true
	}
	if len(tail) >= 9 && string(tail[:9]) == "LYRICS200" {
		return true
	}
	return false
}

func abs64(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// frameSyncAt 报告 offset 是否为 0xFFE 帧同步前缀。
func frameSyncAt(data []byte, off int) bool {
	return data[off] == 0xFF && data[off+1]&0xE0 == 0xE0
}

// resync 在 [from+1, from+window] 内寻找下一个可信帧起点：
// 该处帧头合法且「下一帧的帧头也合法」（双确认，避免垃圾里的假同步）。
// 返回新扫描位置；找不到 ok=false。
func resync(data []byte, from int, streamSampleRate, maxKbps int) (int, bool) {
	limit := from + 64*1024
	if limit > len(data)-4 {
		limit = len(data) - 4
	}
	for o := from + 1; o <= limit; o++ {
		if !frameSyncAt(data, o) {
			continue
		}
		fl, sr, ok := parseFrameHeader(data, o, maxKbps)
		if !ok || (streamSampleRate != 0 && sr != streamSampleRate) {
			continue
		}
		// 双确认：下一帧头也要合法
		nxt := o + fl
		if nxt+4 > len(data) {
			// 尾部最后一段：本帧合法即接受（残余交给尾部判定）
			return o, true
		}
		if frameSyncAt(data, nxt) {
			if _, sr2, ok2 := parseFrameHeader(data, nxt, maxKbps); ok2 && (streamSampleRate == 0 || sr2 == streamSampleRate) {
				return o, true
			}
		}
	}
	return 0, false
}

// parseFrameHeader 解析并校验帧头，返回 (帧长, 采样率, ok)。
func parseFrameHeader(data []byte, off int, maxKbps int) (int, int, bool) {
	b1, b2 := data[off+1], data[off+2]
	version := (b1 >> 3) & 0x03
	layer := (b1 >> 1) & 0x03
	if version == 0x01 || layer != 0x01 {
		return 0, 0, false
	}
	brIdx := int((b2 >> 4) & 0x0F)
	srIdx := int((b2 >> 2) & 0x03)
	padding := int((b2 >> 1) & 0x01)
	var bitrate [16]int
	var sampleTbl [4]int
	switch version {
	case 3:
		copy(bitrate[:], mp3BitrateMPEG1[:])
		sampleTbl = mp3SampleMPEG1
	case 2:
		copy(bitrate[:], mp3BitrateMPEG2[:])
		sampleTbl = mp3SampleMPEG2
	case 0:
		copy(bitrate[:], mp3BitrateMPEG2[:])
		sampleTbl = mp3SampleMPEG25
	}
	if brIdx == 0 || brIdx == 15 || bitrate[brIdx] == 0 {
		return 0, 0, false
	}
	if srIdx == 3 || sampleTbl[srIdx] == 0 {
		return 0, 0, false
	}
	if bitrate[brIdx] > maxKbps {
		return 0, 0, false
	}
	frameLen := (mp3SamplesPerMPEG[version]/8)*bitrate[brIdx]*1000/sampleTbl[srIdx] + padding
	if frameLen <= 0 {
		return 0, 0, false
	}
	return frameLen, sampleTbl[srIdx], true
}

// containsTagMarker 报告尾部残余里是否含可识别的标签标记
// （ID3v1 TAG / APEv2 / Lyrics3 / 结尾 ID3v2）——厂商垃圾包裹的标签也算。
func containsTagMarker(tail []byte) bool {
	for _, marker := range []string{"TAG", "APETAGEX", "LYRICS200", "ID3"} {
		if bytesIndex(tail, marker) {
			return true
		}
	}
	return false
}

func bytesIndex(hay []byte, needle string) bool {
	n := len(needle)
	for i := 0; i+n <= len(hay); i++ {
		match := true
		for j := 0; j < n; j++ {
			if hay[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
