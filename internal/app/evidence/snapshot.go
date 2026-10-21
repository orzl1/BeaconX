package evidence

import (
	"crypto/hmac"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Snapshot 是从一次工具调用中冻结出来的不可变证据快照。
//
// 它只记录元数据；请求/响应正文放在 BlobRef 指向的加密 blob 里。ID 与
// Signature 由 Seal 计算，Verify 负责重算并比对，因此快照可以安全地存进
// 任意能保证字段原样保存的存储（例如数据库）。
type Snapshot struct {
	// ID 是规范化元数据的 SHA-256 十六进制串。
	ID string `json:"id"`
	// MissionID 是快照所属任务。
	MissionID int64 `json:"mission_id"`
	// Tool 是产生这次调用的工具名。
	Tool string `json:"tool"`
	// Request 是请求正文的内容寻址引用。
	Request BlobRef `json:"request"`
	// Response 是响应正文的内容寻址引用。
	Response BlobRef `json:"response"`
	// Redaction 记录正文采用的脱敏策略；可空。
	Redaction string `json:"redaction,omitempty"`
	// CreatedAt 是快照冻结时间，Seal/Verify 一律按 UTC 规范化后再参与计算。
	CreatedAt time.Time `json:"created_at"`
	// Signature 是 HMAC-SHA256(规范化 JSON，不含 Signature) 的十六进制串。
	Signature string `json:"signature,omitempty"`
}

// snapshotMeta 是参与 ID 计算的规范化元数据：不含 ID 与 Signature。
//
// 字段顺序固定、全部为显式结构体字段，不依赖 map 迭代顺序；时间用字符串
// 承载，避免 time.Time 的时区/单调时钟影响序列化结果。
type snapshotMeta struct {
	MissionID int64   `json:"mission_id"`
	Tool      string  `json:"tool"`
	Request   BlobRef `json:"request"`
	Response  BlobRef `json:"response"`
	Redaction string  `json:"redaction,omitempty"`
	CreatedAt string  `json:"created_at"`
}

// snapshotDoc 是参与签名的规范化文档：含 ID、不含 Signature。
//
// 签 ID 而不是只签元数据，是为了让「ID 被单独改动」也属于签名覆盖范围；
// 虽然 ID 本身由元数据推导，但显式覆盖能让校验顺序与失败原因更清晰。
type snapshotDoc struct {
	ID        string  `json:"id"`
	MissionID int64   `json:"mission_id"`
	Tool      string  `json:"tool"`
	Request   BlobRef `json:"request"`
	Response  BlobRef `json:"response"`
	Redaction string  `json:"redaction,omitempty"`
	CreatedAt string  `json:"created_at"`
}

// canonicalTimestamp 返回参与规范化 JSON 的时间字符串。
//
// 固定用 UTC + RFC3339Nano：保留纳秒精度（数据库若按微秒存储，调用方应在
// Seal 之前自行截断），同时消除时区偏移对签名的影响。
func canonicalTimestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// snapshotMetaOf 把快照投影成参与 ID 计算的规范化元数据。
func snapshotMetaOf(snap Snapshot) snapshotMeta {
	return snapshotMeta{
		MissionID: snap.MissionID,
		Tool:      snap.Tool,
		Request:   snap.Request,
		Response:  snap.Response,
		Redaction: snap.Redaction,
		CreatedAt: canonicalTimestamp(snap.CreatedAt),
	}
}

// snapshotDocOf 把快照投影成参与签名的规范化文档。
func snapshotDocOf(id string, snap Snapshot) snapshotDoc {
	return snapshotDoc{
		ID:        id,
		MissionID: snap.MissionID,
		Tool:      snap.Tool,
		Request:   snap.Request,
		Response:  snap.Response,
		Redaction: snap.Redaction,
		CreatedAt: canonicalTimestamp(snap.CreatedAt),
	}
}

// Seal 计算并写回快照的 ID 与 Signature。
//
// Seal 只做规范化与计算，不要求 Request/Response 指向的 blob 已经存在——证据
// 冻结与正文落盘是两个独立步骤，正文是否存在由 GetBlob / Verify 的调用方决定。
// CreatedAt 为零值时取当前时间；非零值统一转换为 UTC。相同输入重复 Seal 结果
// 相同，因此可以从数据库读回快照后重新计算签名来检测篡改。
func (s *Store) Seal(snap Snapshot) (Snapshot, error) {
	if snap.CreatedAt.IsZero() {
		snap.CreatedAt = time.Now()
	}
	snap.CreatedAt = snap.CreatedAt.UTC()

	meta, err := json.Marshal(snapshotMetaOf(snap))
	if err != nil {
		return Snapshot{}, fmt.Errorf("快照元数据规范化失败: %w", err)
	}
	id := sha256Hex(meta)

	doc, err := json.Marshal(snapshotDocOf(id, snap))
	if err != nil {
		return Snapshot{}, fmt.Errorf("快照签名文档规范化失败: %w", err)
	}
	snap.ID = id
	snap.Signature = hex.EncodeToString(hmacDigest(s.macKey, doc))
	return snap, nil
}

// Verify 重算并比对快照的 ID 与签名，不一致时返回 ErrTampered。
//
// Verify 不信任快照里的任何字段：ID 由元数据重算，签名由「重算出的 ID +
// 元数据」重算，因此 ID、元数据、签名任一被改动（或换了一把密钥）都会失败。
func (s *Store) Verify(snap Snapshot) error {
	if snap.ID == "" || snap.Signature == "" {
		return fmt.Errorf("%w：快照缺少 ID 或签名", ErrTampered)
	}

	meta, err := json.Marshal(snapshotMetaOf(snap))
	if err != nil {
		return fmt.Errorf("快照元数据规范化失败: %w", err)
	}
	wantID := sha256Hex(meta)
	if snap.ID != wantID {
		return fmt.Errorf("%w：快照 ID 与元数据不一致", ErrTampered)
	}

	doc, err := json.Marshal(snapshotDocOf(wantID, snap))
	if err != nil {
		return fmt.Errorf("快照签名文档规范化失败: %w", err)
	}
	// 签名必须是 64 位小写十六进制串：大小写变化也算改动，
	// 避免「只改编码不改变字节」绕过篡改检测。
	if !validHash(snap.Signature) {
		return fmt.Errorf("%w：快照签名必须是 %d 位小写十六进制串", ErrTampered, hashHexLen)
	}
	wantMAC := hmacDigest(s.macKey, doc)
	gotMAC, err := hex.DecodeString(snap.Signature)
	if err != nil {
		return fmt.Errorf("%w：快照签名不是合法的十六进制串", ErrTampered)
	}
	if !hmac.Equal(gotMAC, wantMAC) {
		return fmt.Errorf("%w：快照签名不匹配（内容被篡改或密钥不一致）", ErrTampered)
	}
	return nil
}
