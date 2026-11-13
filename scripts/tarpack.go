// tarpack —— 交付 tar.gz 打包辅助(单文件 main,由 release-package.ps1 调用)。
//
// 为什么存在:Windows 打包侧的 tar(System32 bsdtar 不支持 --mode,Git Bash GNU tar
// 又把 "D:\..." 的冒号当远程主机)给所有条目存 0644——Linux 包里的二进制/脚本
// 解压后没有执行位,客户 systemctl start 直接 permission denied(2026-11-13 实测
// 逮住;上轮 relay 包靠 deploy-relay-linux.sh 里的 chmod +x 兜住,agent 包没有这层)。
// 根治:纯 Go archive/tar 打包,模式位按命名规则定死,不依赖任何宿主 tar。
//
// 用法:
//
//	go run scripts/tarpack.go -out reins-agent-linux-vX.tar.gz -C stage/reins-agent-linux file1 file2 ...
//
// 模式规则(确定性,无平台分歧):
//   - 无扩展名的条目(二进制:reins-agent/reins-relay)与 *.sh → 0755
//   - 其余(.service/.md/.json...)→ 0644
//
// gzip 流无 mtime(内容寻址稳定,重打包同内容同字节)。
package main

import (
	"archive/tar"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	out := flag.String("out", "", "输出 tar.gz 路径(必填)")
	root := flag.String("C", "", "切换到的基准目录(条目相对它;默认当前目录)")
	flag.Parse()
	files := flag.Args()
	if *out == "" || len(files) == 0 {
		fmt.Fprintln(os.Stderr, "用法: go run scripts/tarpack.go -out <tar.gz> -C <dir> <file>...")
		os.Exit(2)
	}

	f, err := os.Create(*out)
	if err != nil {
		fail(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	// 零 mtime:同一组输入重打包字节稳定(SHA256 不因重跑漂移)。
	gz.Header.ModTime = time.Time{}
	tw := tar.NewWriter(gz)
	defer tw.Close()

	for _, name := range files {
		full := name
		if *root != "" {
			full = filepath.Join(*root, name)
		}
		info, err := os.Stat(full)
		if err != nil {
			fail(err)
		}
		mode := entryMode(name)
		hdr := &tar.Header{
			Name:     filepath.ToSlash(name),
			Mode:     mode,
			Size:     info.Size(),
			ModTime:  info.ModTime(),
			Typeflag: tar.TypeReg,
			Format:   tar.FormatUSTAR, // 最广兼容(GNU/bsdtar/Go 皆读)
		}
		if err := tw.WriteHeader(hdr); err != nil {
			fail(err)
		}
		src, err := os.Open(full)
		if err != nil {
			fail(err)
		}
		if _, err := io.Copy(tw, src); err != nil {
			src.Close()
			fail(err)
		}
		src.Close()
	}
	fmt.Fprintf(os.Stderr, "tarpack: %s (%d 条目)\n", *out, len(files))
}

// entryMode 模式规则:无扩展名或 .sh → 0755;其余 0644。
func entryMode(name string) int64 {
	if !strings.Contains(filepath.Base(name), ".") || strings.HasSuffix(name, ".sh") {
		return 0o755
	}
	return 0o644
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "tarpack:", err)
	os.Exit(1)
}
