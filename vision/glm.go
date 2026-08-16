// Package vision 封装 GLM 多模态视觉识别，用于 guiclick 执行器。
//
// 移植自 computer-use-rtc/vision.mjs：
//   - OpenAI 兼容的 chat/completions 接口
//   - 图片以 data URL（base64）内联
//   - 返回 JSON 数组：元素含 text/type/x/y/w/h（中心点像素坐标，截图左上为原点）
//   - 用正则容错提取 JSON（模型可能包 ```json 围栏或多余文字）
package vision

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// DefaultBaseURL 智谱 GLM 默认 API 端点。
const DefaultBaseURL = "https://open.bigmodel.cn/api/paas/v4"
const DefaultModel = "glm-5-turbo"

// Client GLM 视觉识别客户端。
type Client struct {
	APIKey  string
	BaseURL string // 默认 DefaultBaseURL
	Model   string // 默认 DefaultModel
	HTTP    *http.Client
}

// New 创建客户端。apiKey 为空时调用会返回错误。
func New(apiKey string) *Client {
	return &Client{
		APIKey:  apiKey,
		BaseURL: DefaultBaseURL,
		Model:   DefaultModel,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

// Element 视觉模型识别出的可交互元素。
type Element struct {
	Text string `json:"text"`
	Type string `json:"type"` // button/input/link/icon/text
	X    int    `json:"x"`    // 中心点 X（像素，截图左上为原点）
	Y    int    `json:"y"`    // 中心点 Y
	W    int    `json:"w"`
	H    int    `json:"h"`
	Desc string `json:"desc,omitempty"`
}

// jsonArrRE 容错提取 JSON 数组（对齐 vision.mjs 的 /\[[\s\S]*\]/）。
var jsonArrRE = regexp.MustCompile(`(?s)\[.*\]`)

// FindElements 让模型在截图中找出所有可交互元素。
// intent 描述当前意图（如"找出所有可点击的按钮和输入框"）。
func (c *Client) FindElements(ctx context.Context, pngBytes []byte, intent string) ([]Element, error) {
	if c.APIKey == "" {
		return nil, fmt.Errorf("未配置 GLM API key")
	}
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes)
	prompt := fmt.Sprintf(`请观察这张屏幕截图。%s
对每个可交互元素，返回 JSON 数组，每个元素格式：
{"text":"元素文字","type":"button|input|link|icon|text","x":中心X,"y":中心Y,"w":宽,"h":高,"desc":"简述"}
坐标为像素绝对值，以截图左上角为原点 (0,0)。只返回 JSON 数组，不要其他文字。`, intent)

	body := map[string]any{
		"model": c.Model,
		"messages": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "image_url", "image_url": map[string]string{"url": dataURL}},
					{"type": "text", "text": prompt},
				},
			},
		},
		"temperature": 0.1,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	url := strings.TrimRight(c.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GLM 请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GLM HTTP %d: %s", resp.StatusCode, string(b))
	}

	var respBody struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&respBody); err != nil {
		return nil, fmt.Errorf("GLM 响应解析失败: %w", err)
	}
	if len(respBody.Choices) == 0 {
		return nil, fmt.Errorf("GLM 返回空 choices")
	}
	content := respBody.Choices[0].Message.Content

	// 容错提取 JSON 数组
	match := jsonArrRE.FindString(content)
	if match == "" {
		return nil, nil // 模型可能返回空或表示无元素
	}
	var elements []Element
	if err := json.Unmarshal([]byte(match), &elements); err != nil {
		return nil, fmt.Errorf("元素 JSON 解析失败: %w (原始: %s)", err, truncate(match, 200))
	}
	return elements, nil
}

// FindElement 返回第一个满足 matcher 的元素。matcher 为 nil 则返回第一个。
func (c *Client) FindElement(ctx context.Context, pngBytes []byte, intent string, matcher func(Element) bool) (*Element, []Element, error) {
	elements, err := c.FindElements(ctx, pngBytes, intent)
	if err != nil {
		return nil, nil, err
	}
	if matcher == nil {
		if len(elements) > 0 {
			return &elements[0], elements, nil
		}
		return nil, elements, nil
	}
	for i := range elements {
		if matcher(elements[i]) {
			return &elements[i], elements, nil
		}
	}
	return nil, elements, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
