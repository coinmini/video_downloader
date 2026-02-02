package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"res-downloader/core/shared"
	"sync"
	"time"

	"github.com/elazarl/goproxy"
	gonanoid "github.com/matoous/go-nanoid/v2"
)

var errRateLimit = errors.New("rate limited")

func isRateLimitErr(err error) bool {
	return errors.Is(err, errRateLimit)
}

type KuaishouPlugin struct {
	bridge      *shared.Bridge
	cookies     string
	cookiesMu   sync.RWMutex
	cookieVer   int64 // incremented each time cookies are updated
	cancelFunc  context.CancelFunc
	cancelMu    sync.Mutex
	isFetching  bool
}

func (p *KuaishouPlugin) SetBridge(bridge *shared.Bridge) {
	p.bridge = bridge
}

func (p *KuaishouPlugin) Domains() []string {
	return []string{"kuaishou.com"}
}

func (p *KuaishouPlugin) OnRequest(r *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
	if cookieHeader := r.Header.Get("Cookie"); cookieHeader != "" {
		p.cookiesMu.Lock()
		p.cookies = cookieHeader
		p.cookieVer++
		p.cookiesMu.Unlock()
	}
	return nil, nil
}

func (p *KuaishouPlugin) OnResponse(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
	return nil
}

func (p *KuaishouPlugin) GetCookies() string {
	p.cookiesMu.RLock()
	defer p.cookiesMu.RUnlock()
	return p.cookies
}

func (p *KuaishouPlugin) getCookieVer() int64 {
	p.cookiesMu.RLock()
	defer p.cookiesMu.RUnlock()
	return p.cookieVer
}

func (p *KuaishouPlugin) HasCookies() bool {
	p.cookiesMu.RLock()
	defer p.cookiesMu.RUnlock()
	return p.cookies != ""
}

func (p *KuaishouPlugin) IsFetching() bool {
	p.cancelMu.Lock()
	defer p.cancelMu.Unlock()
	return p.isFetching
}

func (p *KuaishouPlugin) CancelFetch() {
	p.cancelMu.Lock()
	defer p.cancelMu.Unlock()
	if p.cancelFunc != nil {
		p.cancelFunc()
		p.cancelFunc = nil
		p.isFetching = false
	}
}

func (p *KuaishouPlugin) FetchProfileVideos(userId string) error {
	cookies := p.GetCookies()
	if cookies == "" {
		return fmt.Errorf("no cookies available, please browse kuaishou.com first")
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

		pcursor := ""
		totalFetched := 0
		pageNum := 0
		currentCookies := cookies
		lastCookieVer := p.getCookieVer()

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

			videos, nextCursor, err := p.fetchPage(ctx, userId, pcursor, currentCookies)

			if err != nil && isRateLimitErr(err) {
				// Rate limited — wait for user to refresh browser page to get new cookies
				p.bridge.Send("batchFetchProgress", map[string]interface{}{
					"status":  "waiting_cookies",
					"total":   totalFetched,
					"message": fmt.Sprintf("被限流，已获取 %d 个视频。请在浏览器中刷新快手页面以获取新Cookie，将自动继续...", totalFetched),
				})

				// Poll every 3 seconds for new cookies (up to 5 minutes)
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

				// Retry the same page with new cookies
				continue
			}

			if err != nil {
				p.bridge.Send("batchFetchProgress", map[string]interface{}{
					"status":  "error",
					"total":   totalFetched,
					"message": err.Error(),
				})
				return
			}

			pageNum++
			for _, video := range videos {
				totalFetched++
				p.emitVideo(video)
			}

			p.bridge.Send("batchFetchProgress", map[string]interface{}{
				"status":  "fetching",
				"total":   totalFetched,
				"page":    pageNum,
				"message": fmt.Sprintf("正在获取中... 已获取 %d 个视频", totalFetched),
			})

			if nextCursor == "no_more" || nextCursor == "" || len(videos) == 0 {
				break
			}
			pcursor = nextCursor

			select {
			case <-ctx.Done():
				p.bridge.Send("batchFetchProgress", map[string]interface{}{
					"status":  "cancelled",
					"total":   totalFetched,
					"message": fmt.Sprintf("已取消，共获取 %d 个视频", totalFetched),
				})
				return
			case <-time.After(5 * time.Second):
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

type graphqlRequest struct {
	OperationName string      `json:"operationName"`
	Variables     interface{} `json:"variables"`
	Query         string      `json:"query"`
}

type profilePhotoListVars struct {
	UserId  string `json:"userId"`
	Pcursor string `json:"pcursor"`
	Page    string `json:"page"`
}

const profilePhotoListQuery = `query visionProfilePhotoList($userId: String, $pcursor: String, $page: String) {
  visionProfilePhotoList(userId: $userId, pcursor: $pcursor, page: $page) {
    result
    llsid
    webPageArea
    feeds {
      type
      author {
        id
        name
        headerUrl
        __typename
      }
      photo {
        ... on PhotoEntity {
          id
          duration
          caption
          likeCount
          realLikeCount
          commentCount
          viewCount
          coverUrl
          photoUrl
          liked
          timestamp
          expTag
          animatedCoverUrl
          stereoType
          videoRatio
          __typename
        }
      }
      canAddComment
      currentPcursor
      llsid
      status
      __typename
    }
    hostName
    pcursor
    __typename
  }
}`

func (p *KuaishouPlugin) fetchPage(ctx context.Context, userId, pcursor, cookies string) ([]map[string]interface{}, string, error) {
	reqBody := graphqlRequest{
		OperationName: "visionProfilePhotoList",
		Variables: profilePhotoListVars{
			UserId:  userId,
			Pcursor: pcursor,
			Page:    "profile",
		},
		Query: profilePhotoListQuery,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, "", fmt.Errorf("marshal request failed: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://www.kuaishou.com/graphql", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, "", fmt.Errorf("create request failed: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", cookies)
	req.Header.Set("Origin", "https://www.kuaishou.com")
	req.Header.Set("Referer", "https://www.kuaishou.com/profile/"+userId)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

	// Use a transport that bypasses system proxy to avoid routing through our own proxy
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read response failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// Truncate body for error message
		bodyPreview := string(respBody)
		if len(bodyPreview) > 500 {
			bodyPreview = bodyPreview[:500]
		}
		return nil, "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, bodyPreview)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, "", fmt.Errorf("parse response failed: %w", err)
	}

	// Handle top-level error response (no "data" field)
	if resultCode, ok := result["result"].(float64); ok && resultCode != 1 {
		return nil, "", fmt.Errorf("API error: result=%v (Cookie 可能已过期或被限流): %w", resultCode, errRateLimit)
	}

	data, ok := result["data"].(map[string]interface{})
	if !ok {
		return nil, "", fmt.Errorf("unexpected response format: no data field")
	}

	photoList, ok := data["visionProfilePhotoList"].(map[string]interface{})
	if !ok {
		return nil, "", fmt.Errorf("unexpected response format: no visionProfilePhotoList")
	}

	if resultCode, ok := photoList["result"].(float64); ok && resultCode != 1 {
		return nil, "", fmt.Errorf("API error: result=%v (Cookie 可能已过期或被限流): %w", resultCode, errRateLimit)
	}

	nextCursor := ""
	if pc, ok := photoList["pcursor"].(string); ok {
		nextCursor = pc
	}

	feeds, ok := photoList["feeds"].([]interface{})
	if !ok {
		return nil, nextCursor, nil
	}

	var videos []map[string]interface{}
	for _, feed := range feeds {
		if feedMap, ok := feed.(map[string]interface{}); ok {
			videos = append(videos, feedMap)
		}
	}

	return videos, nextCursor, nil
}

func (p *KuaishouPlugin) emitVideo(feed map[string]interface{}) {
	photo, ok := feed["photo"].(map[string]interface{})
	if !ok {
		return
	}

	photoUrl, _ := photo["photoUrl"].(string)
	if photoUrl == "" {
		return
	}

	coverUrl, _ := photo["coverUrl"].(string)
	caption, _ := photo["caption"].(string)

	urlSign := shared.Md5(photoUrl)
	if p.bridge.MediaIsMarked(urlSign) {
		return
	}

	id, err := gonanoid.New()
	if err != nil {
		id = urlSign
	}

	otherData := map[string]string{}
	for _, key := range []string{"likeCount", "viewCount"} {
		if raw, exists := photo[key]; exists && raw != nil {
			switch v := raw.(type) {
			case float64:
				otherData[key] = fmt.Sprintf("%.0f", v)
			case string:
				otherData[key] = v
			}
		}
	}

	res := shared.MediaInfo{
		Id:          id,
		Url:         photoUrl,
		UrlSign:     urlSign,
		CoverUrl:    coverUrl,
		Size:        0,
		Domain:      "kuaishou.com",
		Classify:    "video",
		Suffix:      ".mp4",
		Status:      shared.DownloadStatusReady,
		SavePath:    "",
		DecodeKey:   "",
		OtherData:   otherData,
		Description: caption,
		ContentType: "video/mp4",
	}

	p.bridge.MarkMedia(urlSign)
	p.bridge.Send("newResources", res)
}
