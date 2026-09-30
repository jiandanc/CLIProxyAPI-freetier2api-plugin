package minimaxcode

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// isUnreserved 判断是否为 JavaScript encodeURIComponent 不转义的字符集。
// JS 规范字符集：A-Z a-z 0-9 - _ . ! ~ * ' ( )
func isUnreserved(b byte) bool {
	return (b >= 'A' && b <= 'Z') ||
		(b >= 'a' && b <= 'z') ||
		(b >= '0' && b <= '9') ||
		b == '-' || b == '_' || b == '.' ||
		b == '!' || b == '~' || b == '*' ||
		b == '\'' || b == '(' || b == ')'
}

// EncodeURIComponent 模拟 JavaScript 的 encodeURIComponent 行为。
// 对非 ASCII 字符按 UTF-8 字节编码为大写 %XX 形式。
func EncodeURIComponent(value string) string {
	var buf strings.Builder
	for i := 0; i < len(value); i++ {
		b := value[i]
		if isUnreserved(b) {
			buf.WriteByte(b)
		} else {
			fmt.Fprintf(&buf, "%%%02X", b)
		}
	}
	return buf.String()
}

// FormEncode 模拟 URLSearchParams 的表单键值编码。
// 字符集与 EncodeURIComponent 一致，但空格转义为 +。
func FormEncode(value string) string {
	return strings.ReplaceAll(EncodeURIComponent(value), "%20", "+")
}

// MD5Hex 计算字符串的小写 32 位 MD5 哈希。
func MD5Hex(value string) string {
	h := md5.Sum([]byte(value))
	return hex.EncodeToString(h[:])
}

// XSignature 计算 x-signature 请求头：MD5(unix_seconds + salt + body)。
func XSignature(unixSeconds int64, body string) string {
	return MD5Hex(fmt.Sprintf("%d%s%s", unixSeconds, SignatureSalt, body))
}

// YYSignature 计算 yy 请求头：MD5(encodeURIComponent(sign_url) + "_" + body + MD5(ms) + "ooui")。
func YYSignature(signURL, body string, unixMS int64) string {
	msMD5 := MD5Hex(strconv.FormatInt(unixMS, 10))
	input := EncodeURIComponent(signURL) + "_" + body + msMD5 + SignatureSuffix
	return MD5Hex(input)
}
