package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// probeParams 是协商探测请求的最小参数（1 样本、T=0、极短输出）。
var probeParams = RequestParams{
	Model:       "", // 探测不依赖模型名；部分端点必填，由调用方在探测前补齐（见 Negotiate 注释）
	Prompt:      "OK",
	Temperature: 0,
	MaxTokens:   2,
}

// Negotiate 执行协议协商（设计 §6.3 四步）：
//  1. 显式指定优先：protocol 非 auto 直接锁定，失败不自动换协议；
//  2. auto：按 base_url 特征初判候选顺序；
//  3. 能力探测：最小请求（1 样本、T=0）按状态码分类——
//     200 锁定；401/403 密钥错误不降级；404/405 协议不匹配换下一候选；400 请求体问题中止；
//  4. 连续三候选失败 → protocol-undetermined 告警错误。
//
// 协商失败即中止（不进 Complete 重试矩阵）：探测请求是元操作，瞬时 5xx/429
// 由上层（collector）在审计级重试或下个会话重试（M2.1 验收语义，审查 R-M3）。
// 每个探测请求计入 RPM/RPD 限流预算（避免协商风暴绕过预算）。
//
// model 参数：探测请求的模型名（由调用方提供，如试点模型）。空串时部分端点可能 400，
// 该行为符合"400 中止"语义（探测参数错误不换协议）。
// 并发安全：全程持 c.mu，仅 auto 且未协商时真正协商；其余调用等待锁定结果。
func (c *Client) Negotiate(ctx context.Context, model string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.protocol != ProtocolAuto {
		return nil // 显式锁定，不协商
	}
	if err := validateBaseURL(c.cfg.BaseURL); err != nil {
		return err
	}
	probe := probeParams
	probe.Model = model

	var lastErr error
	for _, cand := range candidateProtocols(c.cfg.BaseURL) {
		if err := c.rate.Wait(ctx); err != nil {
			return err
		}
		status, err := c.probe(ctx, cand, probe)
		if err != nil {
			// 网络/传输错误：记录并尝试下一候选
			lastErr = fmt.Errorf("provider: 探测 %s 网络错误: %w", cand, err)
			continue
		}
		switch {
		case status == http.StatusOK:
			c.protocol = cand
			return nil
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			// 密钥错误：不降级（避免把 A 厂商密钥误配到恶意端点后静默换协议）
			return fmt.Errorf("%w: %s 返回 %d（%s）", ErrKeyRejected, cand, status, c.cfg.Name)
		case status == http.StatusBadRequest:
			// 请求体/参数问题：换协议会掩盖真实 bug，中止
			return fmt.Errorf("%w: %s 返回 400（%s）", ErrBadRequest, cand, c.cfg.Name)
		case status == http.StatusNotFound || status == http.StatusMethodNotAllowed:
			// 协议不匹配：换下一候选
			lastErr = fmt.Errorf("provider: %s 返回 %d（协议不匹配）", cand, status)
			continue
		case status >= 500 || status == http.StatusTooManyRequests:
			// 5xx/429：端点存在但暂不可用/限流——不换协议（避免瞬时故障锁错协议），
			// 协商中止，由上层审计级重试
			return fmt.Errorf("%w: %s 返回 %d（端点暂不可用或限流）", ErrUndetermined, cand, status)
		default:
			lastErr = fmt.Errorf("provider: %s 返回未预期状态 %d", cand, status)
			continue
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%w: 无候选协议", ErrUndetermined)
	}
	return fmt.Errorf("%w: %s（%s）", ErrUndetermined, c.cfg.Name, lastErr)
}

// probe 发送单个探测请求并返回 HTTP 状态码（响应体最多读 1MB 后排空，防带宽滥用）。
func (c *Client) probe(ctx context.Context, p Protocol, rp RequestParams) (int, error) {
	req, err := BuildHTTPRequest(c.cfg.BaseURL, p, rp, c.cfg.APIKey(), c.cfg.Headers)
	if err != nil {
		return 0, err
	}
	req = req.WithContext(ctx)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	// 关闭前排空（限量），释放连接
	_, _ = io.CopyN(io.Discard, resp.Body, 1<<20)
	return resp.StatusCode, nil
}

// candidateProtocols 按 base_url 特征确定探测候选顺序（§6.3 第 2 步）：
//   - api.openai.com → responses 优先，chat 次之
//   - api.anthropic.com → anthropic
//   - 其它 → responses, chat, anthropic
func candidateProtocols(baseURL string) []Protocol {
	b := strings.ToLower(baseURL)
	switch {
	case strings.Contains(b, "anthropic.com"):
		return []Protocol{ProtocolAnthropic}
	case strings.Contains(b, "api.openai.com"):
		return []Protocol{ProtocolResponses, ProtocolChat}
	default:
		return []Protocol{ProtocolResponses, ProtocolChat, ProtocolAnthropic}
	}
}
