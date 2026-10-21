package evidence

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixedTime 是快照测试使用的固定时间（含纳秒，用于锁定 RFC3339Nano 形状）。
func fixedTime() time.Time {
	return time.Date(2026, 10, 21, 7, 0, 0, 123456789, time.UTC)
}

// testSnapshot 返回一个字段齐全、尚未 Seal 的快照。
func testSnapshot() Snapshot {
	return Snapshot{
		MissionID: 7,
		Tool:      "http_probe",
		Request:   BlobRef{Hash: strings.Repeat("a", hashHexLen), Size: 3},
		Response:  BlobRef{Hash: strings.Repeat("b", hashHexLen), Size: 4},
		Redaction: "headers+json",
		CreatedAt: fixedTime(),
	}
}

func mustSeal(t *testing.T, st *Store, snap Snapshot) Snapshot {
	t.Helper()
	sealed, err := st.Seal(snap)
	if err != nil {
		t.Fatalf("Seal：%v", err)
	}
	return sealed
}

func TestSealVerifyRoundTrip(t *testing.T) {
	st := newTestStore(t, 1)
	sealed := mustSeal(t, st, testSnapshot())

	if len(sealed.ID) != hashHexLen {
		t.Errorf("ID 长度 = %d，期望 %d", len(sealed.ID), hashHexLen)
	}
	if len(sealed.Signature) != hashHexLen {
		t.Errorf("签名长度 = %d，期望 %d", len(sealed.Signature), hashHexLen)
	}
	if !sealed.CreatedAt.Equal(fixedTime()) {
		t.Errorf("CreatedAt = %v，期望 %v", sealed.CreatedAt, fixedTime())
	}
	if err := st.Verify(sealed); err != nil {
		t.Fatalf("Verify：%v", err)
	}

	// Seal 幂等：对已签名的快照再算一次，ID 与签名都不变。
	resealed := mustSeal(t, st, sealed)
	if resealed.ID != sealed.ID || resealed.Signature != sealed.Signature {
		t.Error("重复 Seal 应得到相同的 ID 与签名")
	}
	if err := st.Verify(resealed); err != nil {
		t.Fatalf("重复 Seal 后 Verify：%v", err)
	}
}

func TestSealIsDeterministic(t *testing.T) {
	st := newTestStore(t, 1)
	first := mustSeal(t, st, testSnapshot())
	second := mustSeal(t, st, testSnapshot())
	if first.ID != second.ID {
		t.Errorf("相同元数据的 ID 应相同：%s vs %s", first.ID, second.ID)
	}
	if first.Signature != second.Signature {
		t.Error("相同元数据的签名应相同")
	}
}

func TestSealNormalizesTimeZone(t *testing.T) {
	st := newTestStore(t, 1)
	utc := testSnapshot()

	// 同一时刻用 +08:00 时区表达，规范化后必须与 UTC 输入完全一致。
	plus8 := utc
	plus8.CreatedAt = fixedTime().In(time.FixedZone("UTC+8", 8*60*60))
	sealedUTC := mustSeal(t, st, utc)
	sealedPlus8 := mustSeal(t, st, plus8)

	if sealedPlus8.ID != sealedUTC.ID || sealedPlus8.Signature != sealedUTC.Signature {
		t.Error("同一时刻的不同时区表示应得到相同的 ID 与签名")
	}
	if loc := sealedPlus8.CreatedAt.Location(); loc != time.UTC {
		t.Errorf("Seal 后 CreatedAt 时区 = %v，期望 UTC", loc)
	}
	if err := st.Verify(sealedPlus8); err != nil {
		t.Fatalf("Verify：%v", err)
	}

	// 从数据库读回时时区可能变成 Local，Verify 仍应接受（同一时刻）。
	reloaded := sealedUTC
	reloaded.CreatedAt = reloaded.CreatedAt.In(time.FixedZone("Local", 0))
	if err := st.Verify(reloaded); err != nil {
		t.Fatalf("同一时刻的其它时区表示应通过 Verify：%v", err)
	}
}

func TestSealFillsZeroTime(t *testing.T) {
	st := newTestStore(t, 1)
	snap := testSnapshot()
	snap.CreatedAt = time.Time{}
	sealed := mustSeal(t, st, snap)
	if sealed.CreatedAt.IsZero() {
		t.Fatal("Seal 应为零值时间填入当前时间")
	}
	if loc := sealed.CreatedAt.Location(); loc != time.UTC {
		t.Errorf("CreatedAt 时区 = %v，期望 UTC", loc)
	}
	if err := st.Verify(sealed); err != nil {
		t.Fatalf("Verify：%v", err)
	}
}

func TestVerifyRejectsUnsealedSnapshot(t *testing.T) {
	st := newTestStore(t, 1)
	if err := st.Verify(testSnapshot()); !errors.Is(err, ErrTampered) {
		t.Fatalf("未 Seal 的快照应返回 ErrTampered，实际：%v", err)
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	// 每个用例从同一份已签名快照出发，只改动一个字段。
	mutations := []struct {
		name   string
		mutate func(*Snapshot)
	}{
		{name: "ID", mutate: func(s *Snapshot) { s.ID = flipHex(s.ID) }},
		{name: "任务标识", mutate: func(s *Snapshot) { s.MissionID++ }},
		{name: "工具名", mutate: func(s *Snapshot) { s.Tool += "-tampered" }},
		{name: "请求哈希", mutate: func(s *Snapshot) { s.Request.Hash = flipHex(s.Request.Hash) }},
		{name: "请求大小", mutate: func(s *Snapshot) { s.Request.Size++ }},
		{name: "响应哈希", mutate: func(s *Snapshot) { s.Response.Hash = flipHex(s.Response.Hash) }},
		{name: "响应大小", mutate: func(s *Snapshot) { s.Response.Size++ }},
		{name: "脱敏策略", mutate: func(s *Snapshot) { s.Redaction = "none" }},
		{name: "创建时间", mutate: func(s *Snapshot) { s.CreatedAt = s.CreatedAt.Add(time.Nanosecond) }},
		{name: "签名", mutate: func(s *Snapshot) { s.Signature = flipHex(s.Signature) }},
		{name: "签名大写", mutate: func(s *Snapshot) { s.Signature = "A" + s.Signature[1:] }},
		{name: "签名清空", mutate: func(s *Snapshot) { s.Signature = "" }},
		{name: "ID 大写", mutate: func(s *Snapshot) { s.ID = "A" + s.ID[1:] }},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t, 1)
			sealed := mustSeal(t, st, testSnapshot())
			tc.mutate(&sealed)
			if err := st.Verify(sealed); !errors.Is(err, ErrTampered) {
				t.Fatalf("篡改 %s 后应返回 ErrTampered，实际：%v", tc.name, err)
			}
		})
	}
}

// flipHex 把十六进制串的首字符换成另一个合法字符。
func flipHex(hash string) string {
	if hash == "" {
		return "0"
	}
	replacement := byte('0')
	if hash[0] == '0' {
		replacement = '1'
	}
	return string(replacement) + hash[1:]
}

func TestVerifyAcrossStoresWithDifferentKeys(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "evidence")
	stA, err := New(Config{Dir: dir, Key: testKey(1)})
	if err != nil {
		t.Fatalf("New A：%v", err)
	}
	stB, err := New(Config{Dir: dir, Key: testKey(2)})
	if err != nil {
		t.Fatalf("New B：%v", err)
	}

	sealedA := mustSeal(t, stA, testSnapshot())
	sealedB := mustSeal(t, stB, testSnapshot())

	if sealedA.ID != sealedB.ID {
		t.Error("ID 只由元数据决定，跨密钥应相同")
	}
	if sealedA.Signature == sealedB.Signature {
		t.Error("不同密钥的签名不应相同")
	}
	if err := stB.Verify(sealedA); !errors.Is(err, ErrTampered) {
		t.Fatalf("用 B 的密钥验证 A 的签名应返回 ErrTampered，实际：%v", err)
	}
	if err := stA.Verify(sealedB); !errors.Is(err, ErrTampered) {
		t.Fatalf("用 A 的密钥验证 B 的签名应返回 ErrTampered，实际：%v", err)
	}
}

func TestVerifyAfterJSONRoundTrip(t *testing.T) {
	st := newTestStore(t, 1)
	sealed := mustSeal(t, st, testSnapshot())
	raw, err := json.Marshal(sealed)
	if err != nil {
		t.Fatalf("序列化快照：%v", err)
	}
	var reloaded Snapshot
	if err := json.Unmarshal(raw, &reloaded); err != nil {
		t.Fatalf("反序列化快照：%v", err)
	}
	if err := st.Verify(reloaded); err != nil {
		t.Fatalf("JSON 往返后 Verify：%v", err)
	}
}

func TestSnapshotCanonicalJSONShape(t *testing.T) {
	st := newTestStore(t, 1)
	sealed := mustSeal(t, st, testSnapshot())

	meta, err := json.Marshal(snapshotMetaOf(sealed))
	if err != nil {
		t.Fatalf("序列化规范化元数据：%v", err)
	}
	wantMeta := `{"mission_id":7,"tool":"http_probe",` +
		`"request":{"hash":"` + strings.Repeat("a", hashHexLen) + `","size":3},` +
		`"response":{"hash":"` + strings.Repeat("b", hashHexLen) + `","size":4},` +
		`"redaction":"headers+json","created_at":"2026-10-21T07:00:00.123456789Z"}`
	if string(meta) != wantMeta {
		t.Fatalf("规范化元数据形状不符合预期：\n得到：%s\n期望：%s", meta, wantMeta)
	}
	if want := sha256Hex(meta); sealed.ID != want {
		t.Errorf("ID = %s，期望规范化元数据的 SHA-256 %s", sealed.ID, want)
	}

	doc, err := json.Marshal(snapshotDocOf(sealed.ID, sealed))
	if err != nil {
		t.Fatalf("序列化签名文档：%v", err)
	}
	wantDoc := `{"id":"` + sealed.ID + `",` + strings.TrimPrefix(wantMeta, "{")
	if string(doc) != wantDoc {
		t.Fatalf("签名文档形状不符合预期：\n得到：%s\n期望：%s", doc, wantDoc)
	}
}

func TestSnapshotOmitsEmptyRedaction(t *testing.T) {
	st := newTestStore(t, 1)
	snap := testSnapshot()
	snap.Redaction = ""
	sealed := mustSeal(t, st, snap)
	meta, err := json.Marshal(snapshotMetaOf(sealed))
	if err != nil {
		t.Fatalf("序列化规范化元数据：%v", err)
	}
	if strings.Contains(string(meta), "redaction") {
		t.Errorf("空脱敏策略不应出现在规范化 JSON 中：%s", meta)
	}
	if err := st.Verify(sealed); err != nil {
		t.Fatalf("Verify：%v", err)
	}
}

func TestSnapshotJSONFields(t *testing.T) {
	// 锁定对外 JSON 契约：字段名与编码方式是数据库/接口依赖的稳定形状。
	sealed := mustSeal(t, newTestStore(t, 1), testSnapshot())
	raw, err := json.Marshal(sealed)
	if err != nil {
		t.Fatalf("序列化快照：%v", err)
	}
	for _, field := range []string{
		`"id":`, `"mission_id":`, `"tool":`, `"request":`, `"response":`,
		`"redaction":`, `"created_at":`, `"signature":`,
	} {
		if !strings.Contains(string(raw), field) {
			t.Errorf("快照 JSON 缺少字段 %s：%s", field, raw)
		}
	}
}
