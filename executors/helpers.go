package executors

import "encoding/json"

// jsonUnmarshal json.Unmarshal 的别名，集中便于 mock 测试。
func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// jsonMarshal json.Marshal 别名。
func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

// b64enc / b64dec base64 编解码辅助（测试用）。
func b64enc(s string) string {
	return base64StdEncoding(s)
}
func b64dec(s string) string {
	return base64StdDecode(s)
}
