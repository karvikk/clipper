package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var windowHandle uintptr

func hideConsoleWindow(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000,
	}
}

func applyBorderlessWindow(handle unsafe.Pointer) error {
	windowHandle = uintptr(handle)
	getWindowLong := syscall.NewLazyDLL("user32.dll").NewProc("GetWindowLongPtrW")
	setWindowLong := syscall.NewLazyDLL("user32.dll").NewProc("SetWindowLongPtrW")
	setWindowPosition := syscall.NewLazyDLL("user32.dll").NewProc("SetWindowPos")
	const styleIndex = ^uintptr(15)
	style, _, callErr := getWindowLong.Call(windowHandle, styleIndex)
	if style == 0 && callErr != syscall.Errno(0) {
		return fmt.Errorf("не удалось прочитать стиль окна: %w", callErr)
	}
	const (
		windowCaption    = 0x00C00000
		windowThickFrame = 0x00040000
		dwmwaBorderColor = 34
		dwmwaColorNone   = 0xFFFFFFFE
	)
	previous, _, callErr := setWindowLong.Call(windowHandle, styleIndex, style&^windowCaption)
	if previous == 0 && callErr != syscall.Errno(0) {
		return fmt.Errorf("не удалось убрать системную рамку: %w", callErr)
	}
	borderColor := uint32(dwmwaColorNone)
	borderResult, _, _ := syscall.NewLazyDLL("dwmapi.dll").NewProc("DwmSetWindowAttribute").Call(
		windowHandle,
		dwmwaBorderColor,
		uintptr(unsafe.Pointer(&borderColor)),
		unsafe.Sizeof(borderColor),
	)
	if int32(borderResult) != 0 {
		previous, _, callErr = setWindowLong.Call(windowHandle, styleIndex, style&^windowCaption&^windowThickFrame)
		if previous == 0 && callErr != syscall.Errno(0) {
			return fmt.Errorf("не удалось убрать светлую рамку окна: %w", callErr)
		}
	}
	const frameChanged = 0x0020
	const noMoveNoSizeNoZOrder = 0x0001 | 0x0002 | 0x0004
	result, _, callErr := setWindowPosition.Call(windowHandle, 0, 0, 0, 0, 0, noMoveNoSizeNoZOrder|frameChanged)
	if result == 0 {
		return fmt.Errorf("не удалось применить стиль окна: %w", callErr)
	}
	return nil
}

func beginWindowDrag() error {
	if windowHandle == 0 {
		return errors.New("окно ещё не готово")
	}
	user32 := syscall.NewLazyDLL("user32.dll")
	_, _, _ = user32.NewProc("ReleaseCapture").Call()
	const (
		wmNonClientLeftButtonDown = 0x00A1
		hitTestCaption            = 2
	)
	user32.NewProc("SendMessageW").Call(windowHandle, wmNonClientLeftButtonDown, hitTestCaption, 0)
	return nil
}

func minimizeWindow() error {
	if windowHandle == 0 {
		return errors.New("окно ещё не готово")
	}
	const showMinimized = 6
	result, _, callErr := syscall.NewLazyDLL("user32.dll").NewProc("ShowWindow").Call(windowHandle, showMinimized)
	if result == 0 && callErr != syscall.Errno(0) {
		return fmt.Errorf("не удалось свернуть окно: %w", callErr)
	}
	return nil
}

func toggleMaximizeWindow() error {
	if windowHandle == 0 {
		return errors.New("окно ещё не готово")
	}
	user32 := syscall.NewLazyDLL("user32.dll")
	isZoomed, _, _ := user32.NewProc("IsZoomed").Call(windowHandle)
	show := uintptr(3)
	if isZoomed != 0 {
		show = 9
	}
	_, _, callErr := user32.NewProc("ShowWindow").Call(windowHandle, show)
	if callErr != syscall.Errno(0) {
		return fmt.Errorf("не удалось изменить размер окна: %w", callErr)
	}
	return nil
}

func closeWindow() error {
	if windowHandle == 0 {
		return errors.New("окно ещё не готово")
	}
	const wmClose = 0x0010
	result, _, callErr := syscall.NewLazyDLL("user32.dll").NewProc("PostMessageW").Call(windowHandle, wmClose, 0, 0)
	if result == 0 {
		return fmt.Errorf("не удалось закрыть окно: %w", callErr)
	}
	return nil
}

func openTelegramContact() error {
	if err := exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", "https://t.me/karvikk").Start(); err != nil {
		return fmt.Errorf("не удалось открыть Telegram: %w", err)
	}
	return nil
}

func protectLicenseKey(key string) ([]byte, error) {
	if key == "" {
		return nil, errors.New("ключ доступа пуст")
	}
	inputBytes := []byte(key)
	input := windows.DataBlob{Size: uint32(len(inputBytes)), Data: &inputBytes[0]}
	var output windows.DataBlob
	if err := windows.CryptProtectData(&input, nil, nil, 0, nil, 0, &output); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data)))
	return append([]byte(nil), unsafe.Slice(output.Data, output.Size)...), nil
}

func unprotectLicenseKey(encrypted []byte) (string, error) {
	if len(encrypted) == 0 {
		return "", errors.New("файл ключа пуст")
	}
	input := windows.DataBlob{Size: uint32(len(encrypted)), Data: &encrypted[0]}
	var output windows.DataBlob
	if err := windows.CryptUnprotectData(&input, nil, nil, 0, nil, 0, &output); err != nil {
		return "", err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data)))
	return string(unsafe.Slice(output.Data, output.Size)), nil
}

func writeFileAtomically(path string, data []byte, mode os.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}
