package evidence

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// 通知渠道敏感配置的加密辅助（B8）。
//
// 复用证据主密钥派生的独立子密钥（label 带用途/版本），避免与 blob 加密共用
// 同一把子密钥；AAD 固定为用途标签，密文不能跨用途搬运。格式：
//
//	enc:v1:<base64url(nonce||AES-256-GCM 密文)>
//
// 注意：子密钥由 encKey 派生，因此 RotateKey 之后旧密文无法解密；轮换流程必须
// 同步重加密通知渠道配置（当前由 HTTP 层在轮换后处理），否则读取会失败。
const (
	secretKeyLabel = "beaconx/notification/secret/v1"
	secretAAD      = "beaconx/notification/secret"
	secretPrefix   = "enc:v1:"
	secretKeySize  = 32
)

// EncryptSecret 用通知子密钥加密一段敏感文本（空串原样返回）。
func (s *Store) EncryptSecret(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	s.mu.RLock()
	encKey := append([]byte(nil), s.encKey...)
	s.mu.RUnlock()
	if len(encKey) != keySize {
		return "", fmt.Errorf("证据密钥未初始化")
	}
	key := deriveKey(encKey, secretKeyLabel)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("初始化通知配置加密器失败: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("初始化通知配置加密模式失败: %w", err)
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("生成通知配置加密随机数失败: %w", err)
	}
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), []byte(secretAAD))
	return secretPrefix + base64.RawURLEncoding.EncodeToString(ciphertext), nil
}

// DecryptSecret 解密 EncryptSecret 的产物。
func (s *Store) DecryptSecret(token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", nil
	}
	if !strings.HasPrefix(token, secretPrefix) {
		return "", errors.New("通知配置密文格式不合法")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, secretPrefix))
	if err != nil {
		return "", fmt.Errorf("通知配置密文编码不合法: %w", err)
	}
	if len(raw) < nonceSize+tagSize {
		return "", errors.New("通知配置密文长度不足")
	}
	s.mu.RLock()
	encKey := append([]byte(nil), s.encKey...)
	s.mu.RUnlock()
	key := deriveKey(encKey, secretKeyLabel)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("初始化通知配置解密器失败: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("初始化通知配置解密模式失败: %w", err)
	}
	plain, err := gcm.Open(nil, raw[:nonceSize], raw[nonceSize:], []byte(secretAAD))
	if err != nil {
		return "", fmt.Errorf("通知配置解密失败：密钥不匹配或密文被篡改")
	}
	return string(plain), nil
}
