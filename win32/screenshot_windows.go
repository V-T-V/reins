//go:build windows

package win32

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"syscall"
	"unsafe"
)

var (
	gdi32                  = syscall.NewLazyDLL("gdi32.dll")
	procGetDC              = user32.NewProc("GetDC")
	procReleaseDC          = user32.NewProc("ReleaseDC")
	procCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject       = gdi32.NewProc("SelectObject")
	procDeleteObject       = gdi32.NewProc("DeleteObject")
	procDeleteDC           = gdi32.NewProc("DeleteDC")
	procBitBlt             = gdi32.NewProc("BitBlt")
	procGetDIBits          = gdi32.NewProc("GetDIBits")
	procPrintWindow        = user32.NewProc("PrintWindow")
)

const (
	srccopy     = 0x00CC0020
	capbltFlags = 0 // GetDIBits 用
)

// BITMAPINFOHEADER 用于 GetDIBits。
type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

// CaptureWindow 截取指定窗口。返回 PNG 字节。
//
// 优先用 PrintWindow + PW_RENDERFULLCONTENT（截 Chromium/Electron 不黑屏），
// 失败则降级 BitBlt（CopyFromScreen 等价）。
func CaptureWindow(hwnd uintptr, fullDesktop bool) ([]byte, error) {
	if fullDesktop {
		return captureDesktop()
	}
	if hwnd == 0 {
		return captureDesktop()
	}
	// 先尝试 PrintWindow
	if pngBytes, err := captureByPrintWindow(hwnd); err == nil {
		return pngBytes, nil
	}
	// 降级 BitBlt
	rect := getRect(hwnd)
	return captureByBitBlt(int(rect.Left), int(rect.Top), rect.Width(), rect.Height())
}

// captureByPrintWindow 用 PrintWindow + PW_RENDERFULLCONTENT 截图。
func captureByPrintWindow(hwnd uintptr) ([]byte, error) {
	rect := getRect(hwnd)
	w, h := rect.Width(), rect.Height()
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("窗口尺寸异常 %dx%d", w, h)
	}

	hdcWin, _, _ := procGetDC.Call(hwnd)
	if hdcWin == 0 {
		return nil, fmt.Errorf("GetDC 失败")
	}
	defer procReleaseDC.Call(hwnd, hdcWin)

	hdcMem, _, _ := procCreateCompatibleDC.Call(hdcWin)
	if hdcMem == 0 {
		return nil, fmt.Errorf("CreateCompatibleDC 失败")
	}
	defer procDeleteDC.Call(hdcMem)

	hBmp, _, _ := procCreateCompatibleBitmap.Call(hdcWin, uintptr(w), uintptr(h))
	if hBmp == 0 {
		return nil, fmt.Errorf("CreateCompatibleBitmap 失败")
	}
	defer procDeleteObject.Call(hBmp)

	old, _, _ := procSelectObject.Call(hdcMem, hBmp)
	defer procSelectObject.Call(hdcMem, old)

	// PrintWindow(hwnd, hdcMem, PW_RENDERFULLCONTENT)
	r, _, _ := procPrintWindow.Call(hwnd, hdcMem, pwRenderFullContent)
	if r == 0 {
		return nil, fmt.Errorf("PrintWindow 失败")
	}
	return readBitmap(hdcMem, hBmp, w, h)
}

// captureDesktop 截取整个虚拟桌面主屏。
func captureDesktop() ([]byte, error) {
	w, h, err := ScreenSize()
	if err != nil {
		return nil, err
	}
	return captureByBitBlt(0, 0, w, h)
}

// captureByBitBlt 用 BitBlt 从屏幕拷贝（CopyFromScreen 等价）。
func captureByBitBlt(x, y, w, h int) ([]byte, error) {
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("区域尺寸异常 %dx%d", w, h)
	}
	hdcScreen, _, _ := procGetDC.Call(0)
	if hdcScreen == 0 {
		return nil, fmt.Errorf("GetDC(0) 失败")
	}
	defer procReleaseDC.Call(0, hdcScreen)

	hdcMem, _, _ := procCreateCompatibleDC.Call(hdcScreen)
	if hdcMem == 0 {
		return nil, fmt.Errorf("CreateCompatibleDC 失败")
	}
	defer procDeleteDC.Call(hdcMem)

	hBmp, _, _ := procCreateCompatibleBitmap.Call(hdcScreen, uintptr(w), uintptr(h))
	if hBmp == 0 {
		return nil, fmt.Errorf("CreateCompatibleBitmap 失败")
	}
	defer procDeleteObject.Call(hBmp)

	old, _, _ := procSelectObject.Call(hdcMem, hBmp)
	defer procSelectObject.Call(hdcMem, old)

	// BitBlt(hdcMem, 0, 0, w, h, hdcScreen, x, y, SRCCOPY)
	r, _, _ := procBitBlt.Call(hdcMem, 0, 0, uintptr(w), uintptr(h), hdcScreen, uintptr(x), uintptr(y), srccopy)
	if r == 0 {
		return nil, fmt.Errorf("BitBlt 失败")
	}
	return readBitmap(hdcMem, hBmp, w, h)
}

// readBitmap 从 HBITMAP 读取像素数据并编码为 PNG。
func readBitmap(hdc, hBmp uintptr, w, h int) ([]byte, error) {
	bmi := bitmapInfoHeader{
		Size:     uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		Width:    int32(w),
		Height:   int32(-h), // 负高 = top-down，省去翻转
		Planes:   1,
		BitCount: 32,
	}
	buf := make([]byte, w*h*4)
	ret, _, _ := procGetDIBits.Call(hdc, hBmp, 0, uintptr(h),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&bmi)), 0)
	if ret == 0 {
		return nil, fmt.Errorf("GetDIBits 失败")
	}

	// BGRA → RGBA
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < w*h; i++ {
		off := i * 4
		img.Pix[off+0] = buf[off+2] // R
		img.Pix[off+1] = buf[off+1] // G
		img.Pix[off+2] = buf[off+0] // B
		img.Pix[off+3] = 0xFF       // A
	}

	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
