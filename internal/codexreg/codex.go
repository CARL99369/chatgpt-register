package codexreg

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

const (
	agentVersion     = "0.138.0-alpha.6"
	agentHarnessID   = "codex-cli"
	runningLocation  = "local"
	agentRegisterURL = "https://auth.openai.com/api/accounts/v1/agent/register"
)

// buildAgentIdentity 用 accessToken 完成 Codex Agent Identity 注册，返回完整 auth.json 结构。
// 注册请求走与浏览器相同的代理出口，保持 IP 一致。
func buildAgentIdentity(ctx context.Context, in Input, accessToken string) (map[string]any, error) {
	in.logf("📋 解码 JWT 获取账号信息...")
	accountID, userID, email, planType, err := decodeJWTClaims(accessToken)
	if err != nil {
		return nil, fmt.Errorf("JWT 解码失败: %w", err)
	}

	in.logf("🔐 生成 Ed25519 密钥对...")
	privateKeyB64, publicKeySSH, err := generateEd25519Keypair()
	if err != nil {
		return nil, fmt.Errorf("密钥生成失败: %w", err)
	}

	in.logf("🤖 在 auth.openai.com 注册 agent...")
	agentRuntimeID, err := registerAgent(ctx, in.Proxy, accessToken, publicKeySSH)
	if err != nil {
		return nil, fmt.Errorf("Agent 注册失败: %w", err)
	}
	in.logf("✅ agent_runtime_id=%s", agentRuntimeID)

	if email == "" {
		email = in.Email
	}
	return map[string]any{
		"auth_mode": "agent_identity",
		"agent_identity": map[string]any{
			"agent_runtime_id":           agentRuntimeID,
			"agent_private_key":          privateKeyB64,
			"account_id":                 accountID,
			"chatgpt_user_id":            userID,
			"email":                      email,
			"plan_type":                  planType,
			"chatgpt_account_is_fedramp": false,
		},
	}, nil
}

// decodeJWTClaims 解码 JWT payload（不验证签名），提取账号信息。
func decodeJWTClaims(token string) (accountID, userID, email, planType string, err error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", "", "", "", fmt.Errorf("invalid JWT format")
	}
	payload := parts[1]
	if rem := len(payload) % 4; rem != 0 {
		payload += strings.Repeat("=", 4-rem)
	}
	decoded, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return "", "", "", "", fmt.Errorf("base64 decode: %w", err)
	}
	var claims map[string]any
	if err = json.Unmarshal(decoded, &claims); err != nil {
		return "", "", "", "", fmt.Errorf("json unmarshal: %w", err)
	}
	auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
	profile, _ := claims["https://api.openai.com/profile"].(map[string]any)
	accountID, _ = auth["chatgpt_account_id"].(string)
	userID, _ = auth["chatgpt_user_id"].(string)
	email, _ = profile["email"].(string)
	planType, _ = auth["chatgpt_plan_type"].(string)
	if planType == "" {
		planType = "free"
	}
	return
}

// generateEd25519Keypair 生成 Ed25519 密钥对，返回 (PKCS8 DER base64 私钥, SSH wire format 公钥)。
func generateEd25519Keypair() (privateKeyB64, publicKeySSH string, err error) {
	pubKey, privKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		return "", "", err
	}
	pkcs8Der, err := x509.MarshalPKCS8PrivateKey(privKey)
	if err != nil {
		return "", "", err
	}
	privateKeyB64 = base64.StdEncoding.EncodeToString(pkcs8Der)

	sshHeader := []byte("ssh-ed25519")
	rawPub := []byte(pubKey)
	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.BigEndian, uint32(len(sshHeader)))
	buf.Write(sshHeader)
	_ = binary.Write(buf, binary.BigEndian, uint32(len(rawPub)))
	buf.Write(rawPub)
	publicKeySSH = "ssh-ed25519 " + base64.StdEncoding.EncodeToString(buf.Bytes())
	return
}

// registerAgent 在 auth.openai.com 注册 agent，返回 agent_runtime_id。
func registerAgent(ctx context.Context, proxyURL, accessToken, publicKeySSH string) (string, error) {
	payload := map[string]any{
		"abom": map[string]any{
			"agent_version":    agentVersion,
			"agent_harness_id": agentHarnessID,
			"running_location": runningLocation,
		},
		"agent_public_key": publicKeySSH,
	}
	bodyBytes, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, agentRegisterURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", userAgent)

	client, err := httpClientForProxy(proxyURL)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	var result map[string]any
	if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	agentID, _ := result["agent_runtime_id"].(string)
	if agentID == "" {
		return "", fmt.Errorf("agent_runtime_id not in response: %v", result)
	}
	return agentID, nil
}

// httpClientForProxy 构造经指定代理出口的 HTTP client（空=直连），支持 http(s)/socks5。
func httpClientForProxy(proxyURL string) (*http.Client, error) {
	transport := &http.Transport{}
	if raw := strings.TrimSpace(proxyURL); raw != "" {
		u, err := url.Parse(normalizeProxy(raw))
		if err != nil {
			return nil, fmt.Errorf("代理格式错误: %w", err)
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "https":
			transport.Proxy = http.ProxyURL(u)
		case "socks5", "socks5h":
			var auth *proxy.Auth
			if u.User != nil {
				pw, _ := u.User.Password()
				auth = &proxy.Auth{User: u.User.Username(), Password: pw}
			}
			dialer, derr := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
			if derr != nil {
				return nil, derr
			}
			if cd, ok := dialer.(proxy.ContextDialer); ok {
				transport.DialContext = cd.DialContext
			} else {
				transport.Dial = dialer.Dial //nolint:staticcheck
			}
		default:
			return nil, fmt.Errorf("不支持的代理类型: %s", u.Scheme)
		}
	}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}, nil
}
