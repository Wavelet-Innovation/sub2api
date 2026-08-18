package service

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// 通用透传的 URL 拼接与校验。
//
// 威胁模型：base URL 由管理员在账号上配置（可信），而路径后缀可能来自客户端
// 请求（不可信）。若拼接实现松散，攻击者可以用 ../ 逃出 base 的路径前缀、用
// //evil.com 改写主机、或用绝对 URL 整体替换目标——这三种都会把网关变成 SSRF
// 跳板，且账号里存着真实上游凭证，后果是凭证被转发到攻击者控制的主机。
//
// 因此本文件的规则是"默认拒绝"：只接受不含任何可疑构造的相对路径片段，
// 任何无法明确判定为安全的输入一律报错，绝不做"尽力清洗后放行"。

var (
	errPassthroughEmptyBase      = errors.New("passthrough: base url is required")
	errPassthroughAbsoluteSuffix = errors.New("passthrough: path suffix must be relative")
	errPassthroughTraversal      = errors.New("passthrough: path traversal is not allowed")
	errPassthroughBadEscape      = errors.New("passthrough: path suffix contains an invalid escape sequence")
	errPassthroughControlChar    = errors.New("passthrough: path suffix contains control characters")
)

// buildPassthroughURL 把账号配置的 base URL 与一段上游路径拼成最终请求地址。
//
// upstreamPath 来自匹配命中的路由定义（管理员配置），clientSuffix 是通配路由下
// 客户端提供的剩余片段（不可信，可为空）。
//
// 返回的 URL 保证：主机与 scheme 与 base 完全一致，路径一定位于 base 路径之下。
func buildPassthroughURL(baseURL, upstreamPath, clientSuffix string, rawQuery string) (string, error) {
	trimmedBase := strings.TrimSpace(baseURL)
	if trimmedBase == "" {
		return "", errPassthroughEmptyBase
	}

	parsedBase, err := url.Parse(trimmedBase)
	if err != nil || parsedBase.Scheme == "" || parsedBase.Host == "" {
		return "", fmt.Errorf("passthrough: invalid base url %q", baseURL)
	}
	if scheme := strings.ToLower(parsedBase.Scheme); scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("passthrough: unsupported base url scheme %q", parsedBase.Scheme)
	}

	segments, err := sanitizePassthroughPath(upstreamPath)
	if err != nil {
		return "", err
	}
	suffixSegments, err := sanitizePassthroughPath(clientSuffix)
	if err != nil {
		return "", err
	}
	segments = append(segments, suffixSegments...)

	// 逐段重建路径：只使用通过校验的片段，不复用任何原始字符串，
	// 从根本上排除"校验用的字符串和拼接用的字符串不是同一个"这类绕过。
	basePath := strings.TrimRight(parsedBase.Path, "/")
	finalPath := basePath
	for _, seg := range segments {
		finalPath += "/" + seg
	}

	result := &url.URL{
		Scheme: parsedBase.Scheme,
		Host:   parsedBase.Host,
		User:   parsedBase.User,
		Path:   finalPath,
	}
	if q := strings.TrimSpace(rawQuery); q != "" {
		result.RawQuery = q
	}
	return result.String(), nil
}

// sanitizePassthroughPath 把一段路径拆成安全的片段列表。
//
// 拒绝：绝对 URL（含 scheme）、协议相对地址（//host）、任意形式的 . 与 ..
// （含百分号编码变体）、反斜杠、控制字符与非法转义。空片段被丢弃，因此
// "a//b" 与 "/a/b/" 都归一化为 ["a","b"]。
func sanitizePassthroughPath(raw string) ([]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}

	// 控制字符（含 CR/LF，防请求走私）与反斜杠一律拒绝。
	for _, r := range trimmed {
		if r < 0x20 || r == 0x7f {
			return nil, errPassthroughControlChar
		}
		if r == '\\' {
			return nil, errPassthroughTraversal
		}
	}

	// 绝对 URL 或协议相对地址：会整体改写目标主机。
	if strings.Contains(trimmed, "://") || strings.HasPrefix(trimmed, "//") {
		return nil, errPassthroughAbsoluteSuffix
	}

	// 先解码再判定 ..，否则 %2e%2e 之类的编码变体会漏网。
	decoded, err := url.PathUnescape(trimmed)
	if err != nil {
		return nil, errPassthroughBadEscape
	}
	// 二次解码用于识别双重编码（%252e%252e）。只用于检测，不用于拼接。
	doubleDecoded, err := url.PathUnescape(decoded)
	if err != nil {
		doubleDecoded = decoded
	}
	for _, candidate := range []string{trimmed, decoded, doubleDecoded} {
		if strings.Contains(candidate, "://") || strings.HasPrefix(candidate, "//") {
			return nil, errPassthroughAbsoluteSuffix
		}
		for _, seg := range strings.Split(strings.ReplaceAll(candidate, "\\", "/"), "/") {
			if seg == "." || seg == ".." {
				return nil, errPassthroughTraversal
			}
		}
	}

	segments := make([]string, 0, 8)
	for _, seg := range strings.Split(decoded, "/") {
		if seg == "" {
			continue
		}
		if seg == "." || seg == ".." {
			return nil, errPassthroughTraversal
		}
		// 重新编码，确保片段内的特殊字符不会被解读为路径分隔或查询起始。
		segments = append(segments, url.PathEscape(seg))
	}
	return segments, nil
}
