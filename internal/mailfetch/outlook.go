// Package mailfetch 通过 Microsoft Graph API 拉取 Outlook 邮箱的收件箱邮件，
// 用 refresh_token 换 access_token（scope=.default，按刷新令牌已授权的权限换取），供网页“取件”弹窗展示。
package mailfetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	tokenURL = "https://login.microsoftonline.com/common/oauth2/v2.0/token"
	// 使用 .default：按 refresh_token 原本已授权的权限换取，避免请求未授权 scope 触发 AADSTS70000。
	graphScope = "https://graph.microsoft.com/.default"
	graphBase  = "https://graph.microsoft.com/v1.0"
)

var (
	ErrMissingCreds = errors.New("client_id / refresh_token 必填")
	ErrAuthFailed   = errors.New("邮箱鉴权失败")
	searchFolders   = []string{"Inbox", "JunkEmail"}
)

// Account 一条邮箱凭据。
type Account struct {
	Email        string
	ClientID     string
	RefreshToken string
}

// Message 一封邮件。列表接口只返回头部（ID/发件人/主题/时间），正文按需单独拉取。
type Message struct {
	ID         string    `json:"id"`
	From       string    `json:"from"`
	FromName   string    `json:"from_name"`
	Subject    string    `json:"subject"`
	ReceivedAt time.Time `json:"received_at"`
	HTML       string    `json:"html,omitempty"`
	Text       string    `json:"text,omitempty"`
}

type cachedToken struct {
	access    string
	expiresAt time.Time
}

// Client 无状态可全局复用，内部按 refresh_token 缓存 access_token。
type Client struct {
	http   *http.Client
	tokMu  sync.Mutex
	tokens map[string]cachedToken
}

func New() *Client {
	return &Client{
		http:   &http.Client{Timeout: 15 * time.Second},
		tokens: map[string]cachedToken{},
	}
}

// Verify 校验一条邮箱凭据是否可用：尝试用 refresh_token 换取 access_token。
// 批量并发验证时微软 token 端点会偶发限流/瞬时错误，这里带指数退避重试，避免误判为失败。
func (c *Client) Verify(ctx context.Context, acc Account) error {
	if acc.ClientID == "" || acc.RefreshToken == "" {
		return ErrMissingCreds
	}
	c.invalidate(acc.RefreshToken)
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 800 * time.Millisecond):
			}
		}
		if _, err = c.accessToken(ctx, acc); err == nil {
			return nil
		}
	}
	return err
}

// ListMessages 拉 Inbox + JunkEmail 最新邮件的头部（不含正文），两个文件夹并发拉取，按时间倒序合并返回。
// 正文由 GetMessage 按需单独拉取，避免每次列表都传输大量 HTML 拖慢速度。
func (c *Client) ListMessages(ctx context.Context, acc Account, limit int) ([]Message, error) {
	if limit < 1 {
		limit = 20
	}
	tok, err := c.accessToken(ctx, acc)
	if err != nil {
		return nil, err
	}

	type folderResult struct {
		msgs []graphMessage
	}
	results := make([]folderResult, len(searchFolders))
	var wg sync.WaitGroup
	for i, folder := range searchFolders {
		wg.Add(1)
		go func(i int, folder string) {
			defer wg.Done()
			if msgs, ferr := c.listFolder(ctx, tok, acc, folder, limit); ferr == nil {
				results[i].msgs = msgs
			}
		}(i, folder)
	}
	wg.Wait()

	var all []graphMessage
	for _, r := range results {
		all = append(all, r.msgs...)
	}
	out := make([]Message, 0, len(all))
	for _, m := range all {
		t, _ := time.Parse(time.RFC3339, m.ReceivedDateTime)
		out = append(out, Message{
			ID:         m.ID,
			From:       m.From.EmailAddress.Address,
			FromName:   m.From.EmailAddress.Name,
			Subject:    m.Subject,
			ReceivedAt: t,
		})
	}
	// 合并后按时间倒序
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].ReceivedAt.After(out[i].ReceivedAt) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// GetMessage 按消息 ID 拉取单封邮件的完整正文（HTML + 纯文本）。
func (c *Client) GetMessage(ctx context.Context, acc Account, msgID string) (Message, error) {
	tok, err := c.accessToken(ctx, acc)
	if err != nil {
		return Message{}, err
	}
	q := url.Values{}
	q.Set("$select", "id,subject,body,receivedDateTime,from")
	u := fmt.Sprintf("%s/me/messages/%s?%s", graphBase, url.PathEscape(msgID), q.Encode())

	resp, err := c.graphGet(ctx, tok, u)
	if err != nil {
		return Message{}, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		c.invalidate(acc.RefreshToken)
		newTok, terr := c.accessToken(ctx, acc)
		if terr != nil {
			return Message{}, terr
		}
		if resp, err = c.graphGet(ctx, newTok, u); err != nil {
			return Message{}, err
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return Message{}, fmt.Errorf("graph status=%d", resp.StatusCode)
	}
	var m graphMessage
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return Message{}, err
	}
	html, text := "", ""
	if strings.EqualFold(m.Body.ContentType, "html") {
		html = m.Body.Content
		text = stripHTML(m.Body.Content)
	} else {
		text = m.Body.Content
	}
	t, _ := time.Parse(time.RFC3339, m.ReceivedDateTime)
	return Message{
		ID:         m.ID,
		From:       m.From.EmailAddress.Address,
		FromName:   m.From.EmailAddress.Name,
		Subject:    m.Subject,
		ReceivedAt: t,
		HTML:       html,
		Text:       text,
	}, nil
}

type graphMessage struct {
	ID               string `json:"id"`
	Subject          string `json:"subject"`
	ReceivedDateTime string `json:"receivedDateTime"`
	Body             struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"body"`
	From struct {
		EmailAddress struct {
			Address string `json:"address"`
			Name    string `json:"name"`
		} `json:"emailAddress"`
	} `json:"from"`
}

func (c *Client) listFolder(ctx context.Context, tok string, acc Account, folder string, top int) ([]graphMessage, error) {
	q := url.Values{}
	q.Set("$top", fmt.Sprintf("%d", top))
	q.Set("$orderby", "receivedDateTime desc")
	q.Set("$select", "id,subject,receivedDateTime,from")
	u := fmt.Sprintf("%s/me/mailFolders/%s/messages?%s", graphBase, folder, q.Encode())

	resp, err := c.graphGet(ctx, tok, u)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		c.invalidate(acc.RefreshToken)
		newTok, terr := c.accessToken(ctx, acc)
		if terr != nil {
			return nil, terr
		}
		resp, err = c.graphGet(ctx, newTok, u)
		if err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("graph status=%d", resp.StatusCode)
	}
	var data struct {
		Value []graphMessage `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	return data.Value, nil
}

func (c *Client) graphGet(ctx context.Context, tok, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	return c.http.Do(req)
}

func (c *Client) accessToken(ctx context.Context, acc Account) (string, error) {
	if acc.ClientID == "" || acc.RefreshToken == "" {
		return "", ErrMissingCreds
	}
	c.tokMu.Lock()
	if t, ok := c.tokens[acc.RefreshToken]; ok && time.Until(t.expiresAt) > 60*time.Second {
		c.tokMu.Unlock()
		return t.access, nil
	}
	c.tokMu.Unlock()

	form := url.Values{}
	form.Set("client_id", acc.ClientID)
	form.Set("refresh_token", acc.RefreshToken)
	form.Set("grant_type", "refresh_token")
	form.Set("scope", graphScope)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrAuthFailed, err)
	}
	defer resp.Body.Close()

	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", err
	}
	if tr.Error != "" {
		return "", fmt.Errorf("%w: %s: %s", ErrAuthFailed, tr.Error, tr.ErrorDesc)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("%w: empty access_token", ErrAuthFailed)
	}
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 60 * time.Minute
	}
	c.tokMu.Lock()
	c.tokens[acc.RefreshToken] = cachedToken{access: tr.AccessToken, expiresAt: time.Now().Add(ttl)}
	c.tokMu.Unlock()
	return tr.AccessToken, nil
}

func (c *Client) invalidate(refreshTok string) {
	c.tokMu.Lock()
	delete(c.tokens, refreshTok)
	c.tokMu.Unlock()
}

var (
	htmlTagRe  = regexp.MustCompile(`<[^>]*>`)
	htmlEntity = regexp.MustCompile(`&[a-zA-Z0-9#]+;`)
	wsRe       = regexp.MustCompile(`\s+`)
)

func stripHTML(s string) string {
	s = htmlTagRe.ReplaceAllString(s, " ")
	s = htmlEntity.ReplaceAllString(s, " ")
	return strings.TrimSpace(wsRe.ReplaceAllString(s, " "))
}
