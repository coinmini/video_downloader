// Package xhsign implements Xiaohongshu X-s / X-s-common request signing.
// Ported from https://github.com/Cloxl/xhshow (Python).
package xhsign

import (
	"crypto/md5"
	"crypto/rc4"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/url"
	"strings"
	"time"
)

// --- Constants (from CryptoConfig) ---

var versionBytes = []byte{119, 104, 96, 41}

const (
	hexKey = "71a302257793271ddd273bcee3e4b98d9d7935e1da33f5765e2ea8afb6dc77a51a499d23b67c20660025860cbf13d4540d92497f58686c574e508f46e1956344f39139bf4faf22a3eef120b79258145b2feb5193b6478669961298e79bedca646e1a693a926154a5a7a1bd1cf0dedb742f917a747a1e388b234f2277"

	standardBase64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	customBase64   = "ZmserbBoHQtNP+wOcza/LpngG8yJq42KWYj0DSfdikx3VT16IlUAFM97hECvuRX5"
	x3Base64       = "MfgqrsbcyzPQRStuvC7mn501HIJBo2DEFTKdeNOwxWXYZap89+/A4UVLhijkl63G"

	envFingerprintXorKey = 41
	checksumVersion      = 1
	checksumXorKey       = 115
	x3Prefix             = "mns0301_"
	xysPrefix            = "XYS_"
	b1SecretKey          = "xhswebmplfbt"
)

var checksumFixedTail = []byte{249, 65, 103, 103, 201, 181, 131, 99, 94, 7, 68, 250, 132, 21}

// --- Public API ---

// SignResult holds all the signing headers needed for a XHS API request.
type SignResult struct {
	XS           string `json:"x-s"`
	XSCommon     string `json:"x-s-common"`
	XT           string `json:"x-t"`
	XB3TraceID   string `json:"x-b3-traceid"`
	XXrayTraceID string `json:"x-xray-traceid"`
}

// Sign generates all required signing headers for a XHS API request.
// method: "GET" or "POST"
// uri: API path (e.g. "/api/sns/web/v1/feed") or full URL
// cookies: raw cookie string from browser
// params: query params for GET, or JSON body fields for POST (can be nil)
func Sign(method, uri, cookies string, params map[string]interface{}) (*SignResult, error) {
	cookieMap := parseCookies(cookies)
	a1 := cookieMap["a1"]
	if a1 == "" {
		return nil, fmt.Errorf("missing 'a1' in cookies")
	}

	uri = extractURI(uri)
	ts := float64(time.Now().UnixMilli()) / 1000.0

	// Build content string
	contentStr := buildContentString(method, uri, params)

	// Generate X-s
	xs := signXS(method, uri, a1, contentStr, params, ts)

	// Generate X-s-common
	xsc := signXSCommon(cookieMap)

	// Generate other headers
	xt := fmt.Sprintf("%d", int64(ts*1000))
	b3 := generateB3TraceID()
	xray := generateXrayTraceID(int64(ts * 1000))

	return &SignResult{
		XS:           xs,
		XSCommon:     xsc,
		XT:           xt,
		XB3TraceID:   b3,
		XXrayTraceID: xray,
	}, nil
}

// --- X-s signing ---

func signXS(method, uri, a1, contentStr string, params map[string]interface{}, ts float64) string {
	dValue := md5Hex(contentStr)
	payloadArray := buildPayloadArray(dValue, a1, contentStr, ts)
	xorResult := xorTransformArray(payloadArray)
	x3Sig := encodeX3(xorResult[:124])

	sigData := map[string]string{
		"x0": "4.2.6",
		"x1": "xhs-pc-web",
		"x2": "Windows",
		"x3": x3Prefix + x3Sig,
		"x4": "",
	}

	sigJSON, _ := json.Marshal(sigData)
	return xysPrefix + customEncode(sigJSON)
}

func buildContentString(method, uri string, params map[string]interface{}) string {
	if strings.ToUpper(method) == "POST" {
		if params == nil {
			return uri
		}
		j, _ := json.Marshal(params)
		return uri + string(j)
	}
	// GET
	if params == nil || len(params) == 0 {
		return uri
	}
	parts := make([]string, 0, len(params))
	for k, v := range params {
		vs := fmt.Sprintf("%v", v)
		parts = append(parts, k+"="+url.QueryEscape(vs))
	}
	return uri + "?" + strings.Join(parts, "&")
}

func buildPayloadArray(dValue, a1, contentStr string, ts float64) []byte {
	payload := make([]byte, 0, 128)

	// Version bytes
	payload = append(payload, versionBytes...)

	// Random seed (4 bytes LE)
	seed := rand.Uint32()
	seedBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(seedBytes, seed)
	payload = append(payload, seedBytes...)
	seedByte0 := seedBytes[0]

	// Environment fingerprint A (timestamp XOR'd)
	tsMs := int64(ts * 1000)
	payload = append(payload, envFingerprintA(tsMs, envFingerprintXorKey)...)

	// Environment fingerprint B (page load time)
	timeOffset := rand.Intn(41) + 10 // 10..50
	payload = append(payload, envFingerprintB(int64((ts-float64(timeOffset))*1000))...)

	// Sequence value, window props length, URI length (4 bytes LE each)
	seqVal := rand.Intn(36) + 15    // 15..50
	winProps := rand.Intn(301) + 900 // 900..1200
	uriLen := len(contentStr)

	for _, v := range []int{seqVal, winProps, uriLen} {
		b := make([]byte, 4)
		binary.LittleEndian.PutUint32(b, uint32(v))
		payload = append(payload, b...)
	}

	// MD5 XOR segment (8 bytes)
	md5Bytes := hexToBytes(dValue)
	for i := 0; i < 8; i++ {
		payload = append(payload, md5Bytes[i]^seedByte0)
	}

	// A1 content (length prefix + 52 bytes padded)
	payload = append(payload, 52)
	a1Bytes := []byte(a1)
	if len(a1Bytes) > 52 {
		a1Bytes = a1Bytes[:52]
	}
	for len(a1Bytes) < 52 {
		a1Bytes = append(a1Bytes, 0)
	}
	payload = append(payload, a1Bytes...)

	// Source content (length prefix + 10 bytes padded)
	payload = append(payload, 10)
	srcBytes := []byte("xhs-pc-web")
	if len(srcBytes) > 10 {
		srcBytes = srcBytes[:10]
	}
	for len(srcBytes) < 10 {
		srcBytes = append(srcBytes, 0)
	}
	payload = append(payload, srcBytes...)

	// Trailing bytes
	payload = append(payload, 1)
	payload = append(payload, checksumVersion)
	payload = append(payload, seedByte0^checksumXorKey)
	payload = append(payload, checksumFixedTail...)

	return payload
}

func envFingerprintA(tsMs int64, xorKey byte) []byte {
	data := make([]byte, 8)
	binary.LittleEndian.PutUint64(data, uint64(tsMs))

	sum1 := 0
	for _, b := range data[1:5] {
		sum1 += int(b)
	}
	sum2 := 0
	for _, b := range data[5:8] {
		sum2 += int(b)
	}
	mark := byte((sum1&0xFF + sum2) & 0xFF)
	data[0] = mark

	for i := range data {
		data[i] ^= xorKey
	}
	return data
}

func envFingerprintB(tsMs int64) []byte {
	data := make([]byte, 8)
	binary.LittleEndian.PutUint64(data, uint64(tsMs))
	return data
}

// --- XOR transform ---

func xorTransformArray(src []byte) []byte {
	keyBytes := hexToBytes(hexKey)
	result := make([]byte, len(src))
	for i := range src {
		if i < len(keyBytes) {
			result[i] = src[i] ^ keyBytes[i]
		} else {
			result[i] = src[i]
		}
	}
	return result
}

// --- X-s-common signing ---

func signXSCommon(cookieMap map[string]string) string {
	a1 := cookieMap["a1"]

	fp := generateFingerprint(cookieMap)
	b1 := generateB1(fp)
	x9 := crc32JSInt(b1)

	signStruct := map[string]interface{}{
		"s0":  5,
		"s1":  "",
		"x0":  "1",
		"x1":  "4.2.6",
		"x2":  "Windows",
		"x3":  "xhs-pc-web",
		"x4":  "4.86.0",
		"x5":  a1,
		"x6":  "",
		"x7":  "",
		"x8":  b1,
		"x9":  x9,
		"x10": 0,
		"x11": "normal",
	}

	j, _ := json.Marshal(signStruct)
	return customEncode(j)
}

// --- Fingerprint generation ---

func generateFingerprint(cookieMap map[string]string) map[string]interface{} {
	cookieStr := ""
	for k, v := range cookieMap {
		if cookieStr != "" {
			cookieStr += "; "
		}
		cookieStr += k + "=" + v
	}

	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36 Edg/142.0.0.0"
	tsMs := fmt.Sprintf("%d", time.Now().UnixMilli())
	canvasHash := "124.04347527516074"
	webglHash := randomMD5()
	x53 := randomMD5()

	vendor := "Google Inc. (NVIDIA)"
	renderer := "ANGLE (NVIDIA, NVIDIA GeForce RTX 3060 Direct3D11 vs_5_0 ps_5_0, D3D11)"

	x36 := fmt.Sprintf("%d", rand.Intn(20)+1)

	fp := map[string]interface{}{
		"x1":  ua,
		"x2":  "false",
		"x3":  "zh-CN",
		"x4":  "24",
		"x5":  "8",
		"x6":  "24",
		"x7":  vendor + "," + renderer,
		"x8":  "16",
		"x9":  "1920;1080",
		"x10": "1920;1040",
		"x11": "-480",
		"x12": "Asia/Shanghai",
		"x13": "true",
		"x14": "true",
		"x15": "true",
		"x16": "false",
		"x17": "false",
		"x18": "un",
		"x19": "Win32",
		"x20": "",
		"x21": "PDF Viewer,Chrome PDF Viewer,Chromium PDF Viewer,Microsoft Edge PDF Viewer,WebKit built-in PDF",
		"x22": webglHash,
		"x23": "false",
		"x24": "false",
		"x25": "false",
		"x26": "false",
		"x27": "false",
		"x28": "0,false,false",
		"x29": "4,7,8",
		"x30": "swf object not loaded",
		"x31": canvasHash,
		"x33": "0",
		"x34": "0",
		"x35": "0",
		"x36": x36,
		"x37": "0|0|0|0|0|0|0|0|0|1|0|0|0|0|0|0|0|0|1|0|0|0|0|0",
		"x38": "0|0|1|0|1|0|0|0|0|0|1|0|1|0|1|0|0|0|0|0|0|0|0|0|0|0|0|0|0|0|0|0|0|0|0|0|0|0|0",
		"x39": 0,
		"x40": "0",
		"x41": "0",
		"x42": "3.4.4",
		"x43": canvasHash,
		"x44": tsMs,
		"x45": "__SEC_CAV__1-1-1-1-1|__SEC_WSA__|",
		"x46": "false",
		"x47": "1|0|0|0|0|0",
		"x48": "",
		"x49": "{list:[],type:}",
		"x50": "",
		"x51": "",
		"x52": "",
		"x53": x53,
		"x54": "380,380,360,400,380,400,420,380,400,400,360,360,440,420",
		"x55": "380,380,360,400,380,400,420,380,400,400,360,360,440,420",
		"x56": vendor + "|" + renderer + "|" + webglHash + "|35",
		"x57": cookieStr,
		"x58": "180",
		"x59": "2",
		"x60": "63",
		"x61": "1291",
		"x62": "2047",
		"x63": "0",
		"x64": "0",
		"x65": "0",
		"x66": map[string]interface{}{
			"referer":  "",
			"location": "https://www.xiaohongshu.com/explore",
			"frame":    0,
		},
		"x67": "1|0",
		"x68": "0",
		"x69": "326|1292|30",
		"x70": []string{"location"},
		"x71": "true",
		"x72": "complete",
		"x73": "1191",
		"x74": "0|0|0",
		"x75": "Google Inc.",
		"x76": "true",
		"x77": "1|1|1|1|1|1|1|1|1|1",
		"x78": map[string]interface{}{
			"x":      0,
			"y":      2400,
			"left":   0,
			"right":  290.828125,
			"bottom": 2418,
			"height": 18,
			"top":    2400,
			"width":  290.828125,
			"font":   "Arial,Arial Black,Arial Narrow,Calibri,Cambria,Cambria Math,Comic Sans MS,Consolas,Courier,Courier New,Georgia,Helvetica,Impact,Lucida Console,Lucida Sans Unicode,Microsoft Sans Serif,MS Gothic,MS PGothic,MS Sans Serif,MS Serif,Palatino Linotype,Segoe Print,Segoe Script,Segoe UI,Segoe UI Light,Segoe UI Semibold,Segoe UI Symbol,Tahoma,Times,Times New Roman,Trebuchet MS,Verdana,Wingdings",
		},
		"x79": "144|599565058866",
		"x80": "1|[object FileSystemDirectoryHandle]",
		"x82": "_0x17a2|_0x1954",
	}
	return fp
}

func generateB1(fp map[string]interface{}) string {
	b1FP := map[string]interface{}{}
	for _, k := range []string{"x33", "x34", "x35", "x36", "x37", "x38", "x39", "x42", "x43", "x44", "x45", "x46", "x48", "x49", "x50", "x51", "x52", "x82"} {
		b1FP[k] = fp[k]
	}

	b1JSON, _ := json.Marshal(b1FP)

	cipher, _ := rc4.NewCipher([]byte(b1SecretKey))
	ciphertext := make([]byte, len(b1JSON))
	cipher.XORKeyStream(ciphertext, b1JSON)

	// URL-encode the ciphertext (latin1 interpretation)
	encoded := url.QueryEscape(string(ciphertext))

	// Parse percent-encoded bytes
	var b []byte
	parts := strings.Split(encoded, "%")
	for i, part := range parts {
		if i == 0 {
			for _, c := range part {
				b = append(b, byte(c))
			}
			continue
		}
		if len(part) >= 2 {
			hexVal := 0
			fmt.Sscanf(part[:2], "%x", &hexVal)
			b = append(b, byte(hexVal))
			for _, c := range part[2:] {
				b = append(b, byte(c))
			}
		}
	}

	return customEncode(b)
}

// --- CRC32 (JS-compatible) ---

var crc32Table [256]uint32

func init() {
	const poly uint32 = 0xEDB88320
	for d := uint32(0); d < 256; d++ {
		r := d
		for j := 0; j < 8; j++ {
			if r&1 != 0 {
				r = (r >> 1) ^ poly
			} else {
				r >>= 1
			}
		}
		crc32Table[d] = r
	}
}

func crc32JSInt(data string) int32 {
	var c uint32 = 0xFFFFFFFF

	// JS charCodeAt mode: lower 8 bits of each character
	for _, ch := range data {
		b := byte(ch & 0xFF)
		c = crc32Table[(c^uint32(b))&0xFF] ^ (c >> 8)
	}

	// JS expression: (-1 ^ c ^ 0xEDB88320) >>> 0
	result := (0xFFFFFFFF ^ c) ^ 0xEDB88320

	// Convert to signed 32-bit
	return int32(result)
}

// --- Encoding helpers ---

func customEncode(data []byte) string {
	s := base64.StdEncoding.EncodeToString(data)
	return translateAlphabet(s, standardBase64, customBase64)
}

func encodeX3(data []byte) string {
	s := base64.StdEncoding.EncodeToString(data)
	return translateAlphabet(s, standardBase64, x3Base64)
}

func translateAlphabet(s, from, to string) string {
	result := make([]byte, len(s))
	for i, c := range []byte(s) {
		idx := strings.IndexByte(from, c)
		if idx >= 0 {
			result[i] = to[idx]
		} else {
			result[i] = c // '=' padding etc
		}
	}
	return string(result)
}

// --- Utility helpers ---

func md5Hex(s string) string {
	h := md5.Sum([]byte(s))
	return fmt.Sprintf("%x", h)
}

func randomMD5() string {
	b := make([]byte, 32)
	rand.Read(b)
	h := md5.Sum(b)
	return fmt.Sprintf("%x", h)
}

func hexToBytes(hexStr string) []byte {
	result := make([]byte, len(hexStr)/2)
	for i := 0; i < len(hexStr); i += 2 {
		var b byte
		fmt.Sscanf(hexStr[i:i+2], "%x", &b)
		result[i/2] = b
	}
	return result
}

func parseCookies(raw string) map[string]string {
	m := make(map[string]string)
	for _, pair := range strings.Split(raw, ";") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		idx := strings.IndexByte(pair, '=')
		if idx < 0 {
			continue
		}
		m[pair[:idx]] = pair[idx+1:]
	}
	return m
}

func extractURI(rawURL string) string {
	if strings.HasPrefix(rawURL, "http://") || strings.HasPrefix(rawURL, "https://") {
		u, err := url.Parse(rawURL)
		if err != nil {
			return rawURL
		}
		return u.Path
	}
	return rawURL
}

func generateB3TraceID() string {
	const hexChars = "abcdef0123456789"
	b := make([]byte, 16)
	for i := range b {
		b[i] = hexChars[rand.Intn(len(hexChars))]
	}
	return string(b)
}

func generateXrayTraceID(tsMs int64) string {
	const hexChars = "abcdef0123456789"
	seq := rand.Intn(8388608) // 2^23-1
	part1 := fmt.Sprintf("%016x", (tsMs<<23)|int64(seq))
	b := make([]byte, 16)
	for i := range b {
		b[i] = hexChars[rand.Intn(len(hexChars))]
	}
	return part1 + string(b)
}
