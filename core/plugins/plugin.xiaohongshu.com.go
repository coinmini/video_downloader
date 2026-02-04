package plugins

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"res-downloader/core/shared"
	"res-downloader/core/xhsign"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/elazarl/goproxy"
	gonanoid "github.com/matoous/go-nanoid/v2"
)

// failedVideoInfo stores info for videos that failed to fetch, for later retry
type failedVideoInfo struct {
	noteId       string
	noteUrl      string
	coverUrl     string
	displayTitle string
	xsecToken    string
	otherData    map[string]string
}

type XiaohongshuPlugin struct {
	bridge        *shared.Bridge
	cookies       string
	cookiesMu     sync.RWMutex
	totalNotes    int
	totalMu       sync.Mutex
	fetchSem      chan struct{} // limits concurrent feed API requests
	fetchSemOnce  sync.Once
	pendingVideos sync.Map  // noteId -> true, tracks in-flight video fetches
	pendingCount  int64     // atomic counter of in-flight video fetches

	// Rate limiting
	rateLimitedUntil time.Time  // when rate limited, don't fetch until this time
	rateLimitMu      sync.Mutex // protects rateLimitedUntil

	// Failed video retry queue
	failedVideos []failedVideoInfo
	failedMu     sync.Mutex
}

func (p *XiaohongshuPlugin) SetBridge(bridge *shared.Bridge) {
	p.bridge = bridge
}

func (p *XiaohongshuPlugin) Domains() []string {
	return []string{"xiaohongshu.com", "xhscdn.com"}
}

func (p *XiaohongshuPlugin) OnRequest(r *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
	if r.Host == "edith.xiaohongshu.com" || r.Host == "www.xiaohongshu.com" {
		if cookieHeader := r.Header.Get("Cookie"); cookieHeader != "" {
			p.cookiesMu.Lock()
			p.cookies = cookieHeader
			p.cookiesMu.Unlock()
		}
	}
	return nil, nil
}

func (p *XiaohongshuPlugin) getCookies() string {
	p.cookiesMu.RLock()
	defer p.cookiesMu.RUnlock()
	return p.cookies
}

func (p *XiaohongshuPlugin) OnResponse(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
	if resp == nil || resp.Request == nil || resp.StatusCode != 200 {
		return nil
	}

	if resp.Request.Host != "edith.xiaohongshu.com" {
		return nil
	}

	path := resp.Request.URL.Path
	isUserPosted := strings.Contains(path, "/api/sns/web/v1/user_posted")
	isFeed := strings.Contains(path, "/api/sns/web/v1/feed")

	if !isUserPosted && !isFeed {
		return nil
	}

	jsonBytes, newBody := p.readAndDecodeBody(resp)
	if jsonBytes == nil {
		return nil
	}
	resp.Body = newBody

	if isUserPosted {
		go p.parseUserPostedNotes(jsonBytes)
	} else if isFeed {
		go p.parseFeedNote(jsonBytes)
	}

	return resp
}

func (p *XiaohongshuPlugin) readAndDecodeBody(resp *http.Response) ([]byte, io.ReadCloser) {
	bodyBytes, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, nil
	}

	replacement := io.NopCloser(bytes.NewReader(bodyBytes))

	jsonBytes := bodyBytes
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gr, err := gzip.NewReader(bytes.NewReader(bodyBytes))
		if err == nil {
			decompressed, err := io.ReadAll(gr)
			gr.Close()
			if err == nil {
				jsonBytes = decompressed
			}
		}
	}

	return jsonBytes, replacement
}

// parseUserPostedNotes handles the user_posted API response intercepted when
// the user scrolls through a profile page. Emits both image and video notes.
func (p *XiaohongshuPlugin) parseUserPostedNotes(bodyBytes []byte) {
	notes := p.extractNotes(bodyBytes)
	if notes == nil {
		return
	}

	emitted := 0
	for _, n := range notes {
		if noteMap, ok := n.(map[string]interface{}); ok {
			if p.emitUserPostedNote(noteMap) {
				emitted++
			}
		}
	}

	p.totalMu.Lock()
	p.totalNotes += emitted
	total := p.totalNotes
	p.totalMu.Unlock()

	if emitted > 0 {
		log.Printf("[xiaohongshu] emitted %d notes (total: %d)", emitted, total)
	}

	// After each batch, check if we should retry failed videos
	// Wait for all in-flight fetches to complete, then retry
	go func() {
		// Poll until all pending video fetches are done
		for atomic.LoadInt64(&p.pendingCount) > 0 {
			time.Sleep(2 * time.Second)
		}
		p.retryFailedVideos()
	}()
}

// parseFeedNote handles the feed API response intercepted when the user clicks
// into a note detail page. Extracts the actual video download URL.
func (p *XiaohongshuPlugin) parseFeedNote(bodyBytes []byte) {
	var result map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		return
	}

	data, ok := result["data"].(map[string]interface{})
	if !ok {
		return
	}

	items, ok := data["items"].([]interface{})
	if !ok || len(items) == 0 {
		return
	}

	for _, item := range items {
		itemMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		noteCard, ok := itemMap["note_card"].(map[string]interface{})
		if !ok {
			continue
		}

		noteId, _ := itemMap["id"].(string)
		if noteId == "" {
			continue
		}

		noteType, _ := noteCard["type"].(string)

		if noteType == "video" {
			videoUrl := p.extractVideoUrl(noteCard)
			if videoUrl != "" {
				p.emitFeedVideo(noteCard, noteId, videoUrl)
			}
		} else if noteType == "normal" {
			p.emitFeedImages(noteCard, noteId)
		}
	}
}

func (p *XiaohongshuPlugin) extractVideoUrl(noteCard map[string]interface{}) string {
	video, ok := noteCard["video"].(map[string]interface{})
	if !ok {
		return ""
	}

	media, ok := video["media"].(map[string]interface{})
	if !ok {
		return ""
	}

	stream, ok := media["stream"].(map[string]interface{})
	if !ok {
		return ""
	}

	for _, codec := range []string{"h264", "h265", "av1"} {
		streams, ok := stream[codec].([]interface{})
		if !ok || len(streams) == 0 {
			continue
		}

		for _, s := range streams {
			sm, ok := s.(map[string]interface{})
			if !ok {
				continue
			}
			if masterUrl, ok := sm["master_url"].(string); ok && masterUrl != "" {
				return masterUrl
			}
		}
	}

	return ""
}

func (p *XiaohongshuPlugin) emitFeedVideo(noteCard map[string]interface{}, noteId string, videoUrl string) {
	urlSign := shared.Md5(videoUrl)
	if p.bridge.MediaIsMarked(urlSign) {
		return
	}

	displayTitle, _ := noteCard["title"].(string)
	if displayTitle == "" {
		displayTitle, _ = noteCard["desc"].(string)
	}

	coverUrl := ""
	if imageList, ok := noteCard["image_list"].([]interface{}); ok && len(imageList) > 0 {
		if img, ok := imageList[0].(map[string]interface{}); ok {
			if urlDefault, ok := img["url_default"].(string); ok {
				coverUrl = urlDefault
			}
		}
	}

	likeCount := ""
	if interactInfo, ok := noteCard["interact_info"].(map[string]interface{}); ok {
		likeCount, _ = interactInfo["liked_count"].(string)
	}

	id, err := gonanoid.New()
	if err != nil {
		id = urlSign
	}

	otherData := map[string]string{}
	if likeCount != "" {
		otherData["likeCount"] = likeCount
	}

	// Add download headers to avoid CDN 418 errors
	cookies := p.getCookies()
	downloadHeaders := map[string][]string{
		"Referer":    {"https://www.xiaohongshu.com/"},
		"Origin":     {"https://www.xiaohongshu.com"},
		"Cookie":     {cookies},
		"User-Agent": {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"},
	}
	headersJSON, _ := json.Marshal(downloadHeaders)
	otherData["headers"] = string(headersJSON)

	res := shared.MediaInfo{
		Id:          id,
		Url:         videoUrl,
		UrlSign:     urlSign,
		CoverUrl:    coverUrl,
		Size:        0,
		Domain:      "xiaohongshu.com",
		Classify:    "video",
		Suffix:      ".mp4",
		Status:      shared.DownloadStatusReady,
		SavePath:    "",
		DecodeKey:   "",
		OtherData:   otherData,
		Description: displayTitle,
		ContentType: "video/mp4",
	}

	p.bridge.MarkMedia(urlSign)
	p.bridge.Send("newResources", res)
	log.Printf("[xiaohongshu] emitted video from feed: %s", displayTitle)
}

func (p *XiaohongshuPlugin) emitFeedImages(noteCard map[string]interface{}, noteId string) {
	imageList, ok := noteCard["image_list"].([]interface{})
	if !ok || len(imageList) == 0 {
		return
	}

	displayTitle, _ := noteCard["title"].(string)
	if displayTitle == "" {
		displayTitle, _ = noteCard["desc"].(string)
	}

	likeCount := ""
	if interactInfo, ok := noteCard["interact_info"].(map[string]interface{}); ok {
		likeCount, _ = interactInfo["liked_count"].(string)
	}

	for i, img := range imageList {
		imgMap, ok := img.(map[string]interface{})
		if !ok {
			continue
		}

		imageUrl := ""
		if urlDefault, ok := imgMap["url_default"].(string); ok && urlDefault != "" {
			imageUrl = urlDefault
		} else if infoList, ok := imgMap["info_list"].([]interface{}); ok {
			for _, info := range infoList {
				if infoMap, ok := info.(map[string]interface{}); ok {
					if u, ok := infoMap["url"].(string); ok && u != "" {
						imageUrl = u
						break
					}
				}
			}
		}

		if imageUrl == "" {
			continue
		}

		if strings.HasPrefix(imageUrl, "http://") {
			imageUrl = "https://" + imageUrl[7:]
		}

		urlSign := shared.Md5(imageUrl)
		if p.bridge.MediaIsMarked(urlSign) {
			continue
		}

		id, err := gonanoid.New()
		if err != nil {
			id = urlSign
		}

		title := displayTitle
		if len(imageList) > 1 {
			title = fmt.Sprintf("%s_%d", displayTitle, i+1)
		}

		otherData := map[string]string{}
		if likeCount != "" {
			otherData["likeCount"] = likeCount
		}

		suffix := ".jpg"
		if strings.Contains(imageUrl, ".png") {
			suffix = ".png"
		} else if strings.Contains(imageUrl, ".webp") {
			suffix = ".webp"
		}

		res := shared.MediaInfo{
			Id:          id,
			Url:         imageUrl,
			UrlSign:     urlSign,
			CoverUrl:    imageUrl,
			Size:        0,
			Domain:      "xiaohongshu.com",
			Classify:    "image",
			Suffix:      suffix,
			Status:      shared.DownloadStatusReady,
			SavePath:    "",
			DecodeKey:   "",
			OtherData:   otherData,
			Description: title,
			ContentType: "image/jpeg",
		}

		p.bridge.MarkMedia(urlSign)
		p.bridge.Send("newResources", res)
	}
}

func (p *XiaohongshuPlugin) extractNotes(bodyBytes []byte) []interface{} {
	var result map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		log.Printf("[xiaohongshu] parse response failed: %v", err)
		return nil
	}

	if code, ok := result["code"].(float64); ok && code != 0 {
		return nil
	}

	data, ok := result["data"].(map[string]interface{})
	if !ok {
		return nil
	}

	notesRaw, ok := data["notes"].([]interface{})
	if !ok || len(notesRaw) == 0 {
		return nil
	}

	return notesRaw
}

// emitUserPostedNote emits a note from the user_posted API.
// Image notes: cover image URL used directly (downloadable).
// Video notes: calls feed API with xsec_token to get real video URL.
func (p *XiaohongshuPlugin) emitUserPostedNote(note map[string]interface{}) bool {
	noteId, _ := note["note_id"].(string)
	if noteId == "" {
		return false
	}

	displayTitle, _ := note["display_title"].(string)
	noteType, _ := note["type"].(string)

	// Extract xsec_token from the note (required for feed API)
	xsecToken, _ := note["xsec_token"].(string)
	if noteType == "video" && xsecToken == "" {
		log.Printf("[xiaohongshu] WARNING: no xsec_token found for video note %s", noteId)
	}

	coverUrl := ""
	if cover, ok := note["cover"].(map[string]interface{}); ok {
		if urlDefault, ok := cover["url_default"].(string); ok && urlDefault != "" {
			coverUrl = urlDefault
		} else {
			coverUrl, _ = cover["url"].(string)
		}
	}

	if coverUrl == "" {
		return false
	}

	if strings.HasPrefix(coverUrl, "http://") {
		coverUrl = "https://" + coverUrl[7:]
	}

	likeCount := ""
	if interactInfo, ok := note["interact_info"].(map[string]interface{}); ok {
		likeCount, _ = interactInfo["liked_count"].(string)
	}

	otherData := map[string]string{}
	if likeCount != "" {
		otherData["likeCount"] = likeCount
	}

	if noteType == "video" {
		// Video note: call feed API with xsec_token to get real video URL
		noteUrl := fmt.Sprintf("https://www.xiaohongshu.com/explore/%s", noteId)

		// Skip if already successfully fetched (marked) or currently in-flight
		urlSign := shared.Md5(noteUrl)
		if p.bridge.MediaIsMarked(urlSign) {
			return false
		}
		if _, loaded := p.pendingVideos.LoadOrStore(noteId, true); loaded {
			return false // already being fetched
		}

		atomic.AddInt64(&p.pendingCount, 1)
		go p.fetchAndEmitVideo(noteId, noteUrl, coverUrl, displayTitle, xsecToken, otherData)
		return true
	}

	// Image note: use cover CDN URL directly for download
	urlSign := shared.Md5(coverUrl)
	if p.bridge.MediaIsMarked(urlSign) {
		return false
	}

	id, err := gonanoid.New()
	if err != nil {
		id = urlSign
	}

	suffix := ".jpg"
	if strings.Contains(coverUrl, ".png") {
		suffix = ".png"
	} else if strings.Contains(coverUrl, ".webp") {
		suffix = ".webp"
	}

	res := shared.MediaInfo{
		Id:          id,
		Url:         coverUrl,
		UrlSign:     urlSign,
		CoverUrl:    coverUrl,
		Size:        0,
		Domain:      "xiaohongshu.com",
		Classify:    "image",
		Suffix:      suffix,
		Status:      shared.DownloadStatusReady,
		SavePath:    "",
		DecodeKey:   "",
		OtherData:   otherData,
		Description: displayTitle,
		ContentType: "image/jpeg",
	}

	p.bridge.MarkMedia(urlSign)
	p.bridge.Send("newResources", res)
	return true
}

func (p *XiaohongshuPlugin) acquireFetchSlot() {
	p.fetchSemOnce.Do(func() {
		p.fetchSem = make(chan struct{}, 1) // serial: one feed API request at a time
	})
	p.fetchSem <- struct{}{}
}

func (p *XiaohongshuPlugin) releaseFetchSlot() {
	<-p.fetchSem
}

// isRateLimited checks if we're in the 1-hour cooldown period after a rate limit hit.
// If so, it sleeps until the cooldown expires and returns true.
func (p *XiaohongshuPlugin) waitIfRateLimited() {
	p.rateLimitMu.Lock()
	until := p.rateLimitedUntil
	p.rateLimitMu.Unlock()

	if until.IsZero() || time.Now().After(until) {
		return
	}

	waitDur := time.Until(until)
	log.Printf("[xiaohongshu] 限流冷却中，将在 %s 恢复抓取（等待 %.0f 分钟）",
		until.Format("15:04:05"), waitDur.Minutes())
	time.Sleep(waitDur)
	log.Printf("[xiaohongshu] 限流冷却结束，恢复抓取")
}

// onFeedRateLimited sets a 1-hour cooldown period.
func (p *XiaohongshuPlugin) onFeedRateLimited() {
	p.rateLimitMu.Lock()
	defer p.rateLimitMu.Unlock()
	p.rateLimitedUntil = time.Now().Add(1 * time.Hour)
	log.Printf("[xiaohongshu] 触发限流！将停止抓取1小时，恢复时间: %s", p.rateLimitedUntil.Format("15:04:05"))
}

// fetchAndEmitVideo calls the feed API with signed headers to get the real video URL.
// If successful, emits the CDN video URL and marks it; on failure, adds to retry queue.
func (p *XiaohongshuPlugin) fetchAndEmitVideo(noteId, noteUrl, coverUrl, displayTitle, xsecToken string, otherData map[string]string) {
	defer atomic.AddInt64(&p.pendingCount, -1)

	p.acquireFetchSlot()
	defer p.releaseFetchSlot()

	// Wait if we're in rate limit cooldown (1 hour)
	p.waitIfRateLimited()

	// Fixed 5s delay + random jitter (0-3s)
	jitter := float64(rand.Intn(3000)) / 1000.0
	sleepDur := time.Duration((5.0+jitter)*1000) * time.Millisecond
	time.Sleep(sleepDur)

	cookies := p.getCookies()
	videoUrl := ""
	rateLimited := false
	if cookies == "" {
		log.Printf("[xiaohongshu] no cookies available for %s", noteId)
	} else {
		videoUrl, rateLimited = p.fetchVideoFromFeedAPI(noteId, xsecToken, cookies)
		if rateLimited {
			// Rate limited: don't retry now, add to failed queue for later retry
			log.Printf("[xiaohongshu] rate limited for %s, adding to retry queue", noteId)
		}
	}

	if videoUrl == "" {
		// Failed: add to retry queue instead of silently dropping
		p.pendingVideos.Delete(noteId)
		p.failedMu.Lock()
		p.failedVideos = append(p.failedVideos, failedVideoInfo{
			noteId:       noteId,
			noteUrl:      noteUrl,
			coverUrl:     coverUrl,
			displayTitle: displayTitle,
			xsecToken:    xsecToken,
			otherData:    otherData,
		})
		p.failedMu.Unlock()
		log.Printf("[xiaohongshu] video %s (%s) added to retry queue", noteId, displayTitle)
		return
	}

	// Success: mark as done so it won't be retried
	noteUrlSign := shared.Md5(noteUrl)
	p.bridge.MarkMedia(noteUrlSign)
	p.pendingVideos.Delete(noteId)

	// Add download headers to avoid CDN 418 errors
	downloadHeaders := map[string][]string{
		"Referer":    {"https://www.xiaohongshu.com/"},
		"Origin":     {"https://www.xiaohongshu.com"},
		"Cookie":     {cookies},
		"User-Agent": {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"},
	}
	headersJSON, _ := json.Marshal(downloadHeaders)
	otherData["headers"] = string(headersJSON)

	urlSign := shared.Md5(videoUrl)
	id, err := gonanoid.New()
	if err != nil {
		id = urlSign
	}
	res := shared.MediaInfo{
		Id:          id,
		Url:         videoUrl,
		UrlSign:     urlSign,
		CoverUrl:    coverUrl,
		Size:        0,
		Domain:      "xiaohongshu.com",
		Classify:    "video",
		Suffix:      ".mp4",
		Status:      shared.DownloadStatusReady,
		SavePath:    "",
		DecodeKey:   "",
		OtherData:   otherData,
		Description: displayTitle,
		ContentType: "video/mp4",
	}
	p.bridge.MarkMedia(urlSign)
	p.bridge.Send("newResources", res)
	log.Printf("[xiaohongshu] fetched video URL for note %s: %s", noteId, displayTitle)
}

// fetchVideoFromFeedAPI calls the XHS feed API directly with signed headers
// to get the video download URL for a given note. Returns (videoUrl, rateLimited).
func (p *XiaohongshuPlugin) fetchVideoFromFeedAPI(noteId, xsecToken, cookies string) (string, bool) {
	apiPath := "/api/sns/web/v1/feed"
	apiURL := "https://edith.xiaohongshu.com" + apiPath

	payload := map[string]interface{}{
		"source_note_id": noteId,
		"image_formats":  []string{"jpg", "webp", "avif"},
		"extra":          map[string]interface{}{"need_body_topic": 1},
		"xsec_source":    "pc_feed",
		"xsec_token":     xsecToken,
	}

	signResult, err := xhsign.Sign("POST", apiPath, cookies, payload)
	if err != nil {
		log.Printf("[xiaohongshu] sign failed for %s: %v", noteId, err)
		return "", false
	}

	bodyBytes, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", apiURL, bytes.NewReader(bodyBytes))
	if err != nil {
		log.Printf("[xiaohongshu] create feed request failed for %s: %v", noteId, err)
		return "", false
	}

	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("Cookie", cookies)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Origin", "https://www.xiaohongshu.com")
	req.Header.Set("Referer", "https://www.xiaohongshu.com/")
	req.Header.Set("X-s", signResult.XS)
	req.Header.Set("X-t", signResult.XT)
	req.Header.Set("X-s-common", signResult.XSCommon)
	req.Header.Set("X-b3-traceid", signResult.XB3TraceID)
	req.Header.Set("X-xray-traceid", signResult.XXrayTraceID)

	client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			Proxy: nil, // bypass local proxy
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[xiaohongshu] feed API request failed for %s: %v", noteId, err)
		return "", false
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[xiaohongshu] read feed response failed for %s: %v", noteId, err)
		return "", false
	}

	if resp.StatusCode != 200 {
		log.Printf("[xiaohongshu] feed API status %d for %s: %s", resp.StatusCode, noteId, string(respBody[:min(len(respBody), 200)]))
		if resp.StatusCode == 461 && strings.Contains(string(respBody), "300013") {
			p.onFeedRateLimited()
			return "", true
		}
		return "", false
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		log.Printf("[xiaohongshu] parse feed response failed for %s: %v", noteId, err)
		return "", false
	}

	data, ok := result["data"].(map[string]interface{})
	if !ok {
		log.Printf("[xiaohongshu] feed response missing data for %s", noteId)
		return "", false
	}

	items, ok := data["items"].([]interface{})
	if !ok || len(items) == 0 {
		log.Printf("[xiaohongshu] feed response no items for %s", noteId)
		return "", false
	}

	for _, item := range items {
		itemMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		noteCard, ok := itemMap["note_card"].(map[string]interface{})
		if !ok {
			continue
		}
		videoUrl := p.extractVideoUrl(noteCard)
		if videoUrl != "" {
			return videoUrl, false
		}
	}

	log.Printf("[xiaohongshu] no video URL found in feed response for %s", noteId)
	return "", false
}

// retryFailedVideos retries all videos in the failed queue once.
// Videos that still fail after retry get emitted as fallback links.
func (p *XiaohongshuPlugin) retryFailedVideos() {
	p.failedMu.Lock()
	toRetry := p.failedVideos
	p.failedVideos = nil
	p.failedMu.Unlock()

	if len(toRetry) == 0 {
		return
	}

	log.Printf("[xiaohongshu] retrying %d failed videos...", len(toRetry))

	for _, v := range toRetry {
		p.acquireFetchSlot()

		// Wait if rate limited
		p.waitIfRateLimited()

		// Fixed 5s delay + jitter
		jitter := float64(rand.Intn(3000)) / 1000.0
		sleepDur := time.Duration((5.0+jitter)*1000) * time.Millisecond
		time.Sleep(sleepDur)

		cookies := p.getCookies()
		videoUrl := ""
		if cookies != "" {
			videoUrl, _ = p.fetchVideoFromFeedAPI(v.noteId, v.xsecToken, cookies)
		}

		if videoUrl != "" {
			// Success on retry
			noteUrlSign := shared.Md5(v.noteUrl)
			p.bridge.MarkMedia(noteUrlSign)

			downloadHeaders := map[string][]string{
				"Referer":    {"https://www.xiaohongshu.com/"},
				"Origin":     {"https://www.xiaohongshu.com"},
				"Cookie":     {cookies},
				"User-Agent": {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"},
			}
			headersJSON, _ := json.Marshal(downloadHeaders)
			v.otherData["headers"] = string(headersJSON)

			urlSign := shared.Md5(videoUrl)
			id, err := gonanoid.New()
			if err != nil {
				id = urlSign
			}
			res := shared.MediaInfo{
				Id:          id,
				Url:         videoUrl,
				UrlSign:     urlSign,
				CoverUrl:    v.coverUrl,
				Size:        0,
				Domain:      "xiaohongshu.com",
				Classify:    "video",
				Suffix:      ".mp4",
				Status:      shared.DownloadStatusReady,
				SavePath:    "",
				DecodeKey:   "",
				OtherData:   v.otherData,
				Description: v.displayTitle,
				ContentType: "video/mp4",
			}
			p.bridge.MarkMedia(urlSign)
			p.bridge.Send("newResources", res)
			log.Printf("[xiaohongshu] retry succeeded for %s: %s", v.noteId, v.displayTitle)
		} else {
			// Retry failed: emit fallback link
			p.emitFallbackVideo(v)
		}

		p.releaseFetchSlot()
	}

	log.Printf("[xiaohongshu] retry batch complete")
}

// emitFallbackVideo emits a note's web page URL as a fallback when we can't get the real video URL.
func (p *XiaohongshuPlugin) emitFallbackVideo(v failedVideoInfo) {
	urlSign := shared.Md5(v.noteUrl)
	if p.bridge.MediaIsMarked(urlSign) {
		return
	}

	id, err := gonanoid.New()
	if err != nil {
		id = urlSign
	}

	v.otherData["fetchFailed"] = "true"

	res := shared.MediaInfo{
		Id:          id,
		Url:         v.noteUrl,
		UrlSign:     urlSign,
		CoverUrl:    v.coverUrl,
		Size:        0,
		Domain:      "xiaohongshu.com",
		Classify:    "video",
		Suffix:      ".mp4",
		Status:      shared.DownloadStatusReady,
		SavePath:    "",
		DecodeKey:   "",
		OtherData:   v.otherData,
		Description: v.displayTitle,
		ContentType: "video/mp4",
	}

	p.bridge.MarkMedia(urlSign)
	p.bridge.Send("newResources", res)
	log.Printf("[xiaohongshu] emitted fallback link for %s: %s", v.noteId, v.displayTitle)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
