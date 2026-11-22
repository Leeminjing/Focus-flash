package tool

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	consoleCodePage     = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetOEMCP")
	wideCharToMultiByte = windows.NewLazySystemDLL("kernel32.dll").NewProc("WideCharToMultiByte")
)

// PowerShell 7 reads standard input using the OEM code page when it has no
// shared console. Reject lossy conversion.
func encodeConsoleInput(text string) (string, error) {
	if len(text) == utf8.RuneCountInString(text) {
		return text, nil
	}
	page, _, _ := consoleCodePage.Call()
	if page == 65001 {
		return text, nil
	}
	const noBestFit = 0x400
	characters := utf16.Encode([]rune(text))
	encoded := make([]byte, len(characters)*4)
	var substituted int32
	n, _, err := wideCharToMultiByte.Call(page, noBestFit,
		uintptr(unsafe.Pointer(&characters[0])), uintptr(len(characters)),
		uintptr(unsafe.Pointer(&encoded[0])), uintptr(len(encoded)), 0, uintptr(unsafe.Pointer(&substituted)))
	if n == 0 {
		return "", fmt.Errorf("encode shell stdin: %w", err)
	}
	if substituted != 0 {
		return "", errors.New("shell input contains characters unavailable in the Windows console code page")
	}
	return string(encoded[:n]), nil
}

func decodeConsoleOutput(data []byte) string {
	// Native tools can write UTF-8 regardless of the console code page.
	if utf8.Valid(data) {
		return string(data)
	}
	const oemCodePage = 1
	characters := make([]uint16, len(data))
	n, err := windows.MultiByteToWideChar(oemCodePage, 0, &data[0], int32(len(data)), &characters[0], int32(len(characters)))
	if err != nil {
		return strings.ToValidUTF8(string(data), "\ufffd")
	}
	return string(utf16.Decode(characters[:n]))
}
