package executors

import "encoding/base64"

func base64StdEncoding(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func base64StdDecode(s string) string {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return ""
	}
	return string(b)
}
