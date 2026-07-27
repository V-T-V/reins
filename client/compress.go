// Package client — 文件传输 gzip 压缩(O3:大文件提速)。
//
// UploadCompressed / DownloadCompressed:传输前 gzip 压缩,接收后自动解压。
// 适合文本类大文件(日志/配置/代码),压缩比 5-10x,网络提速显著。
// 二进制文件(已压缩的 zip/exe/dmg)效果有限,不推荐用。
package client

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// UploadCompressed 上传文件(发送前 gzip 压缩)。
// 注:Relay 侧需要支持 gzip 解压(通过 Content-Encoding: gzip),
// 当前 Relay 的 stream-through 模式直接透传 body(不自动解压),
// 所以 Agent 侧收到的是 gzip 数据 — Agent 需在写入前解压。
// 简化:仅在 reader 端压缩,接收端需用 DownloadCompressed 对应解压。
func (s *FileService) UploadCompressed(agentID, remotePath string, reader io.Reader) error {
	// gzip 包装 reader
	gzReader, gzWriter := io.Pipe()
	go func() {
		gz := gzip.NewWriter(gzWriter)
		io.Copy(gz, reader)
		gz.Close()
		gzWriter.Close()
	}()
	return s.Upload(agentID, remotePath, gzReader)
}

// DownloadDecompressed 下载文件(接收后 gzip 解压)。
// 注:Agent 侧需配合 gzip 压缩发送(通过 file transfer 的 Content-Encoding),
// 当前简化:仅在 SDK 侧解压(假设 Agent 发送的是 gzip 数据)。
// 生产推荐:文件本身不加压缩,走 stream-through 原样传;
// 仅在特定场景(如拉日志)用此方法。
func (s *FileService) DownloadDecompressed(agentID, remotePath string, writer io.Writer) error {
	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		err := s.downloadToWriter(agentID, remotePath, pw)
		pw.Close()
		errCh <- err
	}()
	gz, err := gzip.NewReader(pr)
	if err != nil {
		return err
	}
	defer gz.Close()
	_, copyErr := io.Copy(writer, gz)
	if copyErr != nil {
		return copyErr
	}
	return <-errCh
}

// downloadToWriter 内部:下载到 writer(不走文件)。
func (s *FileService) downloadToWriter(agentID, remotePath string, w io.Writer) error {
	q := url.Values{}
	q.Set("agent_id", agentID)
	q.Set("path", remotePath)
	q.Set("transfer_id", "dl-compress")
	u := s.c.baseURL + "/api/transfer/download?" + q.Encode()
	req, err := http.NewRequest("POST", u, nil)
	if err != nil {
		return err
	}
	if s.c.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.c.token)
	}
	resp, err := s.c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("download HTTP %d", resp.StatusCode)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}
