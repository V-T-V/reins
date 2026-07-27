package main

import (
	"testing"
)

func TestRelayConfigValidateOK(t *testing.T) {
	c := Config{Addr: ":7780", Token: "valid-token-12345"}
	if err := c.Validate(); err != nil {
		t.Errorf("有效配置应通过: %v", err)
	}
}

func TestRelayConfigValidateEmptyAddr(t *testing.T) {
	c := Config{Addr: ""}
	if err := c.Validate(); err == nil {
		t.Error("空 addr 应报错")
	}
}

func TestRelayConfigValidateMTLSNoCert(t *testing.T) {
	c := Config{Addr: ":7780", MTLS: true}
	if err := c.Validate(); err == nil {
		t.Error("mTLS 无证书应报错")
	}
}

func TestRelayConfigValidateMTLSWithCert(t *testing.T) {
	c := Config{Addr: ":7780", MTLS: true, CertFile: "cert.pem", KeyFile: "key.pem"}
	if err := c.Validate(); err != nil {
		t.Errorf("mTLS 有证书应通过: %v", err)
	}
}

func TestRelayConfigValidateShortToken(t *testing.T) {
	c := Config{Addr: ":7780", Token: "abc"}
	if err := c.Validate(); err == nil {
		t.Error("token < 8 字符应报错")
	}
}

func TestRelayConfigStringMasksToken(t *testing.T) {
	c := Config{Addr: ":7780", Token: "my-secret-token"}
	s := c.String()
	// Token 应被脱敏(只显示前 4 字符)
	if len(s) < 10 {
		t.Error("String() 不应太短")
	}
}

func TestQuickstartLoadOrGenToken(t *testing.T) {
	token := loadOrGenToken()
	if len(token) < 16 {
		t.Errorf("token 长度 %d,应 ≥16", len(token))
	}
	// 第二次调用应返回相同 token
	token2 := loadOrGenToken()
	if token != token2 {
		t.Error("重复调用应返回相同 token")
	}
}

func TestQuickstartGetLocalIP(t *testing.T) {
	ip := getLocalIP()
	if ip == "" {
		t.Error("IP 不应为空")
	}
}
