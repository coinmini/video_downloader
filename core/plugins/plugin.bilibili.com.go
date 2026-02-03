package plugins

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"res-downloader/core/shared"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/elazarl/goproxy"
	gonanoid "github.com/matoous/go-nanoid/v2"
)

// WBI signing permutation table
var mixinKeyEncTab = []int{
	46, 47, 18, 2, 53, 8, 23, 32, 15, 50, 10, 31, 58, 3, 45, 35, 27, 43, 5, 49,
	33, 9, 42, 19, 29, 28, 14, 39, 12, 38, 41, 13, 37, 48, 7, 16, 24, 55, 40,
	61, 26, 17, 0, 1, 60, 51, 30, 4, 22, 25, 54, 21, 56, 59, 6, 63, 57, 62, 11,
	36, 20, 34, 44, 52,
}

type BilibiliPlugin struct {
	bridge     *shared.Bridge
	cookies    string
	cookiesMu  sync.RWMutex
	cookieVer  int64
	imgKey     string
	subKey     string
	wbiMu      sync.RWMutex
	cancelFunc context.CancelFunc
	cancelMu   sync.Mutex
	isFetching bool
}

func (p *BilibiliPlugin) SetBridge(bridge *shared.Bridge) {
	p.bridge = bridge
}

func (p *BilibiliPlugin) Domains() []string {
	return []string{"bilibili.com"}
}

func (p *BilibiliPlugin) OnRequest(r *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
	if cookieHeader := r.Header.Get("Cookie"); cookieHeader != "" {
		p.cookiesMu.Lock()
		p.cookies = cookieHeader
		p.cookieVer++
		p.cookiesMu.Unlock()
	}
	return nil, nil
}

func (p *BilibiliPlugin) OnResponse(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
	if resp == nil || resp.Request == nil {
		return nil
	}

	// Capture WBI keys from nav API response
	if strings.Contains(resp.Request.URL.Path, "/x/web-interface/nav") {
		p.extractWbiKeys(resp)
	}

	return nil
}

func (p *BilibiliPlugin) extractWbiKeys(resp *http.Response) {
	if resp.Body == nil {
		return
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}
	resp.Body = io.NopCloser(strings.NewReader(string(bodyBytes)))

	var result struct {
		Data struct {
			WbiImg struct {
				ImgUrl string `json:"img_url"`
				SubUrl string `json:"sub_url"`
			} `json:"wbi_img"`
		} `json:"data"`
	}
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		return
	}

	imgUrl := result.Data.WbiImg.ImgUrl
	subUrl := result.Data.WbiImg.SubUrl
	if imgUrl == "" || subUrl == "" {
		return
	}

	// Extract key from URL path: .../xxxxx.png -> xxxxx
	imgKey := extractKeyFromUrl(imgUrl)
	subKey := extractKeyFromUrl(subUrl)
	if imgKey == "" || subKey == "" {
		return
	}

	p.wbiMu.Lock()
	p.imgKey = imgKey
	p.subKey = subKey
	p.wbiMu.Unlock()

	fmt.Printf("[bilibili] WBI keys captured: imgKey=%s, subKey=%s\n", imgKey, subKey)
}

func extractKeyFromUrl(rawUrl string) string {
	// URL format: https://i0.hdslb.com/bfs/wbi/xxxxx.png
	parts := strings.Split(rawUrl, "/")
	if len(parts) == 0 {
		return ""
	}
	filename := parts[len(parts)-1]
	// Remove .png extension
	if idx := strings.LastIndex(filename, "."); idx != -1 {
		return filename[:idx]
	}
	return filename
}

func (p *BilibiliPlugin) GetCookies() string {
	p.cookiesMu.RLock()
	defer p.cookiesMu.RUnlock()
	return p.cookies
}

func (p *BilibiliPlugin) getCookieVer() int64 {
	p.cookiesMu.RLock()
	defer p.cookiesMu.RUnlock()
	return p.cookieVer
}

func (p *BilibiliPlugin) HasCookies() bool {
	p.cookiesMu.RLock()
	defer p.cookiesMu.RUnlock()
	return p.cookies != ""
}

func (p *BilibiliPlugin) HasWbiKeys() bool {
	p.wbiMu.RLock()
	defer p.wbiMu.RUnlock()
	return p.imgKey != "" && p.subKey != ""
}

func (p *BilibiliPlugin) IsFetching() bool {
	p.cancelMu.Lock()
	defer p.cancelMu.Unlock()
	return p.isFetching
}

func (p *BilibiliPlugin) CancelFetch() {
	p.cancelMu.Lock()
	defer p.cancelMu.Unlock()
	if p.cancelFunc != nil {
		p.cancelFunc()
		p.cancelFunc = nil
		p.isFetching = false
	}
}

// getMixinKey generates the mixin key from imgKey + subKey using the permutation table
func (p *BilibiliPlugin) getMixinKey() string {
	p.wbiMu.RLock()
	orig := p.imgKey + p.subKey
	p.wbiMu.RUnlock()

	var b strings.Builder
	for _, idx := range mixinKeyEncTab {
		if idx < len(orig) {
			b.WriteByte(orig[idx])
		}
	}
	result := b.String()
	if len(result) > 32 {
		result = result[:32]
	}
	return result
}

// signParams adds WBI signature (w_rid + wts) to query parameters
func (p *BilibiliPlugin) signParams(params map[string]string) map[string]string {
	mixinKey := p.getMixinKey()
	if mixinKey == "" {
		return params
	}

	// Add timestamp
	params["wts"] = strconv.FormatInt(time.Now().Unix(), 10)

	// Sort keys
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Build query string, sanitizing values
	query := url.Values{}
	for _, k := range keys {
		v := sanitizeWbiValue(params[k])
		params[k] = v
		query.Set(k, v)
	}

	// Calculate w_rid = MD5(queryString + mixinKey)
	queryStr := query.Encode()
	hash := md5.Sum([]byte(queryStr + mixinKey))
	params["w_rid"] = hex.EncodeToString(hash[:])

	return params
}

// sanitizeWbiValue removes characters that are not allowed in WBI signed values
func sanitizeWbiValue(s string) string {
	unwanted := []string{"!", "'", "(", ")", "*"}
	for _, c := range unwanted {
		s = strings.ReplaceAll(s, c, "")
	}
	return s
}

// fetchWbiKeysDirectly fetches WBI keys from the nav API if not already captured
func (p *BilibiliPlugin) fetchWbiKeysDirectly(cookies string) error {
	if p.HasWbiKeys() {
		return nil
	}

	req, err := http.NewRequest("GET", "https://api.bilibili.com/x/web-interface/nav", nil)
	if err != nil {
		return fmt.Errorf("create nav request failed: %w", err)
	}

	req.Header.Set("Cookie", cookies)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Referer", "https://www.bilibili.com/")
	req.Header.Set("Origin", "https://www.bilibili.com")

	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{Proxy: nil},
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("nav request failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read nav response failed: %w", err)
	}

	var result struct {
		Data struct {
			WbiImg struct {
				ImgUrl string `json:"img_url"`
				SubUrl string `json:"sub_url"`
			} `json:"wbi_img"`
		} `json:"data"`
	}
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		return fmt.Errorf("parse nav response failed: %w", err)
	}

	imgKey := extractKeyFromUrl(result.Data.WbiImg.ImgUrl)
	subKey := extractKeyFromUrl(result.Data.WbiImg.SubUrl)
	if imgKey == "" || subKey == "" {
		return fmt.Errorf("failed to extract WBI keys from nav response")
	}

	p.wbiMu.Lock()
	p.imgKey = imgKey
	p.subKey = subKey
	p.wbiMu.Unlock()

	fmt.Printf("[bilibili] WBI keys fetched: imgKey=%s, subKey=%s\n", imgKey, subKey)
	return nil
}

func (p *BilibiliPlugin) FetchProfileVideos(mid string) error {
	cookies := p.GetCookies()
	if cookies == "" {
		return fmt.Errorf("no cookies available, please browse bilibili.com first")
	}

	// Ensure WBI keys are available
	if err := p.fetchWbiKeysDirectly(cookies); err != nil {
		return fmt.Errorf("获取WBI签名密钥失败: %w", err)
	}

	p.cancelMu.Lock()
	if p.isFetching {
		p.cancelMu.Unlock()
		return fmt.Errorf("a fetch operation is already in progress")
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancelFunc = cancel
	p.isFetching = true
	p.cancelMu.Unlock()

	go func() {
		defer func() {
			p.cancelMu.Lock()
			p.isFetching = false
			p.cancelFunc = nil
			p.cancelMu.Unlock()
		}()

		totalFetched := 0
		pageNum := 0
		currentCookies := cookies
		lastCookieVer := p.getCookieVer()
		pageSize := 30

		for {
			select {
			case <-ctx.Done():
				p.bridge.Send("batchFetchProgress", map[string]interface{}{
					"status":  "cancelled",
					"total":   totalFetched,
					"message": fmt.Sprintf("已取消，共获取 %d 个视频", totalFetched),
				})
				return
			default:
			}

			pageNum++
			videos, totalCount, err := p.fetchPage(ctx, mid, pageNum, pageSize, currentCookies)

			if err != nil {
				// Check if it's a rate limit / risk control error
				errMsg := err.Error()
				if strings.Contains(errMsg, "-352") || strings.Contains(errMsg, "-412") || strings.Contains(errMsg, "风控") {
					p.bridge.Send("batchFetchProgress", map[string]interface{}{
						"status":  "waiting_cookies",
						"total":   totalFetched,
						"message": fmt.Sprintf("被风控限制，已获取 %d 个视频。请在浏览器中刷新B站页面以获取新Cookie，将自动继续...", totalFetched),
					})

					refreshed := false
					for i := 0; i < 100; i++ {
						select {
						case <-ctx.Done():
							p.bridge.Send("batchFetchProgress", map[string]interface{}{
								"status":  "cancelled",
								"total":   totalFetched,
								"message": fmt.Sprintf("已取消，共获取 %d 个视频", totalFetched),
							})
							return
						case <-time.After(3 * time.Second):
						}

						newVer := p.getCookieVer()
						if newVer > lastCookieVer {
							currentCookies = p.GetCookies()
							lastCookieVer = newVer
							refreshed = true
							break
						}
					}

					if !refreshed {
						p.bridge.Send("batchFetchProgress", map[string]interface{}{
							"status":  "error",
							"total":   totalFetched,
							"message": fmt.Sprintf("等待新Cookie超时，共获取 %d 个视频", totalFetched),
						})
						return
					}

					// Refresh WBI keys with new cookies
					p.wbiMu.Lock()
					p.imgKey = ""
					p.subKey = ""
					p.wbiMu.Unlock()
					if err := p.fetchWbiKeysDirectly(currentCookies); err != nil {
						p.bridge.Send("batchFetchProgress", map[string]interface{}{
							"status":  "error",
							"total":   totalFetched,
							"message": fmt.Sprintf("刷新WBI密钥失败: %v，共获取 %d 个视频", err, totalFetched),
						})
						return
					}

					p.bridge.Send("batchFetchProgress", map[string]interface{}{
						"status":  "fetching",
						"total":   totalFetched,
						"message": fmt.Sprintf("检测到新Cookie，等待10秒后继续获取... 已获取 %d 个视频", totalFetched),
					})

					select {
					case <-ctx.Done():
						p.bridge.Send("batchFetchProgress", map[string]interface{}{
							"status":  "cancelled",
							"total":   totalFetched,
							"message": fmt.Sprintf("已取消，共获取 %d 个视频", totalFetched),
						})
						return
					case <-time.After(10 * time.Second):
					}

					// Retry the same page
					pageNum--
					continue
				}

				p.bridge.Send("batchFetchProgress", map[string]interface{}{
					"status":  "error",
					"total":   totalFetched,
					"message": err.Error(),
				})
				return
			}

			for i, video := range videos {
				totalFetched++
				p.emitVideo(video, currentCookies)

				// Progress update per video
				if (i+1)%5 == 0 || i == len(videos)-1 {
					p.bridge.Send("batchFetchProgress", map[string]interface{}{
						"status":  "fetching",
						"total":   totalFetched,
						"page":    pageNum,
						"message": fmt.Sprintf("正在获取中... 已获取 %d/%d 个视频", totalFetched, totalCount),
					})
				}

				// Delay between each video to avoid rate limiting
				// Each video requires 2 API calls (view + playurl), so pace them out
				if i < len(videos)-1 {
					select {
					case <-ctx.Done():
						p.bridge.Send("batchFetchProgress", map[string]interface{}{
							"status":  "cancelled",
							"total":   totalFetched,
							"message": fmt.Sprintf("已取消，共获取 %d 个视频", totalFetched),
						})
						return
					case <-time.After(500 * time.Millisecond):
					}
				}
			}

			// Check if we've fetched all pages
			if totalFetched >= totalCount || len(videos) == 0 {
				break
			}

			// Delay between pages
			select {
			case <-ctx.Done():
				p.bridge.Send("batchFetchProgress", map[string]interface{}{
					"status":  "cancelled",
					"total":   totalFetched,
					"message": fmt.Sprintf("已取消，共获取 %d 个视频", totalFetched),
				})
				return
			case <-time.After(2 * time.Second):
			}
		}

		p.bridge.Send("batchFetchProgress", map[string]interface{}{
			"status":  "done",
			"total":   totalFetched,
			"message": fmt.Sprintf("获取完成！共获取 %d 个视频", totalFetched),
		})
	}()

	return nil
}

func (p *BilibiliPlugin) fetchPage(ctx context.Context, mid string, pn, ps int, cookies string) ([]map[string]interface{}, int, error) {
	params := map[string]string{
		"mid":            mid,
		"pn":             strconv.Itoa(pn),
		"ps":             strconv.Itoa(ps),
		"order":          "pubdate",
		"platform":       "web",
		"web_location":   "1550101",
		"dm_img_list":    "[]",
		"dm_img_str":     "V2ViR0wgMS4w",
		"dm_cover_img_str": "QU5HTEUgKEFwcGxlLCBBTkdMRSBNZXRhbCBSZW5kZXJlcjogQXBwbGUgTTEgTWF4LCBVbnNwZWNpZmllZCBWZXJzaW9uKUdvb2dsZSBJbmMuIChBcHBsZSk=",
	}

	// Sign with WBI
	params = p.signParams(params)

	// Build URL
	u, _ := url.Parse("https://api.bilibili.com/x/space/wbi/arc/search")
	q := u.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, 0, fmt.Errorf("create request failed: %w", err)
	}

	req.Header.Set("Cookie", cookies)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Referer", fmt.Sprintf("https://space.bilibili.com/%s/video", mid))
	req.Header.Set("Origin", "https://www.bilibili.com")
	req.Header.Set("Accept", "application/json, text/plain, */*")

	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{Proxy: nil},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("read response failed: %w", err)
	}

	if resp.StatusCode == 412 {
		return nil, 0, fmt.Errorf("HTTP 412: 被风控限制 -412")
	}

	if resp.StatusCode != http.StatusOK {
		bodyPreview := string(bodyBytes)
		if len(bodyPreview) > 500 {
			bodyPreview = bodyPreview[:500]
		}
		return nil, 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, bodyPreview)
	}

	var result struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			List struct {
				Vlist []map[string]interface{} `json:"vlist"`
			} `json:"list"`
			Page struct {
				Count int `json:"count"`
				Pn    int `json:"pn"`
				Ps    int `json:"ps"`
			} `json:"page"`
		} `json:"data"`
	}

	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		return nil, 0, fmt.Errorf("parse response failed: %w", err)
	}

	if result.Code != 0 {
		return nil, 0, fmt.Errorf("API error code=%d message=%s -%d", result.Code, result.Message, result.Code)
	}

	return result.Data.List.Vlist, result.Data.Page.Count, nil
}

// fetchVideoCid gets the cid for a video via /x/web-interface/view
func (p *BilibiliPlugin) fetchVideoCid(bvid, cookies string) (int64, error) {
	apiUrl := fmt.Sprintf("https://api.bilibili.com/x/web-interface/view?bvid=%s", bvid)
	req, err := http.NewRequest("GET", apiUrl, nil)
	if err != nil {
		return 0, err
	}

	req.Header.Set("Cookie", cookies)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Referer", fmt.Sprintf("https://www.bilibili.com/video/%s", bvid))

	client := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{Proxy: nil},
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	var result struct {
		Code int `json:"code"`
		Data struct {
			Cid int64 `json:"cid"`
		} `json:"data"`
	}
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		return 0, err
	}
	if result.Code != 0 || result.Data.Cid == 0 {
		return 0, fmt.Errorf("failed to get cid, code=%d", result.Code)
	}
	return result.Data.Cid, nil
}

// fetchPlayUrl gets the actual video stream URL via /x/player/playurl
func (p *BilibiliPlugin) fetchPlayUrl(bvid string, cid int64, cookies string) (string, int64, error) {
	apiUrl := fmt.Sprintf("https://api.bilibili.com/x/player/playurl?bvid=%s&cid=%d&qn=80&fnval=0&fnver=0&fourk=1&platform=html5",
		bvid, cid)
	req, err := http.NewRequest("GET", apiUrl, nil)
	if err != nil {
		return "", 0, err
	}

	req.Header.Set("Cookie", cookies)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Referer", fmt.Sprintf("https://www.bilibili.com/video/%s", bvid))

	client := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{Proxy: nil},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, err
	}

	var result struct {
		Code int `json:"code"`
		Data struct {
			Durl []struct {
				Url    string `json:"url"`
				Size   int64  `json:"size"`
				Length int    `json:"length"`
			} `json:"durl"`
		} `json:"data"`
	}
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		return "", 0, err
	}
	if result.Code != 0 {
		return "", 0, fmt.Errorf("playurl error code=%d", result.Code)
	}
	if len(result.Data.Durl) == 0 {
		return "", 0, fmt.Errorf("no durl in playurl response")
	}
	return result.Data.Durl[0].Url, result.Data.Durl[0].Size, nil
}

func (p *BilibiliPlugin) emitVideo(video map[string]interface{}, cookies string) {
	bvid, _ := video["bvid"].(string)
	if bvid == "" {
		return
	}

	title, _ := video["title"].(string)
	pic, _ := video["pic"].(string)

	// Ensure pic URL has protocol
	if strings.HasPrefix(pic, "//") {
		pic = "https:" + pic
	}

	// Try to get the real video stream URL
	videoUrl := fmt.Sprintf("https://www.bilibili.com/video/%s", bvid)
	var fileSize int64

	cid, err := p.fetchVideoCid(bvid, cookies)
	if err == nil {
		playUrl, size, err2 := p.fetchPlayUrl(bvid, cid, cookies)
		if err2 == nil && playUrl != "" {
			videoUrl = playUrl
			fileSize = size
		}
	}

	urlSign := shared.Md5(bvid) // Use bvid as dedup key since stream URLs change
	if p.bridge.MediaIsMarked(urlSign) {
		return
	}

	id, err := gonanoid.New()
	if err != nil {
		id = urlSign
	}

	otherData := map[string]string{
		"bvid": bvid,
	}

	// Store referer header for download (bilibili requires it)
	headers := map[string]string{
		"Referer":    fmt.Sprintf("https://www.bilibili.com/video/%s", bvid),
		"User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	}
	if headersJson, err := json.Marshal(headers); err == nil {
		otherData["headers"] = string(headersJson)
	}

	// Extract play count
	if play, ok := video["play"]; ok && play != nil {
		switch v := play.(type) {
		case float64:
			otherData["viewCount"] = fmt.Sprintf("%.0f", v)
		case string:
			otherData["viewCount"] = v
		}
	}

	// Extract comment count as likeCount proxy (bilibili list API doesn't return likes)
	if comment, ok := video["comment"]; ok && comment != nil {
		switch v := comment.(type) {
		case float64:
			otherData["likeCount"] = fmt.Sprintf("%.0f", v)
		case string:
			otherData["likeCount"] = v
		}
	}

	res := shared.MediaInfo{
		Id:          id,
		Url:         videoUrl,
		UrlSign:     urlSign,
		CoverUrl:    pic,
		Size:        float64(fileSize),
		Domain:      "bilibili.com",
		Classify:    "video",
		Suffix:      ".mp4",
		Status:      shared.DownloadStatusReady,
		SavePath:    "",
		DecodeKey:   "",
		OtherData:   otherData,
		Description: title,
		ContentType: "video/mp4",
	}

	p.bridge.MarkMedia(urlSign)
	p.bridge.Send("newResources", res)
}
