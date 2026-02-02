package plugins

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"res-downloader/core/shared"
	"strings"
	"sync"

	"github.com/elazarl/goproxy"
	gonanoid "github.com/matoous/go-nanoid/v2"
)

type XiaohongshuPlugin struct {
	bridge     *shared.Bridge
	totalNotes int
	totalMu    sync.Mutex
}

func (p *XiaohongshuPlugin) SetBridge(bridge *shared.Bridge) {
	p.bridge = bridge
}

func (p *XiaohongshuPlugin) Domains() []string {
	return []string{"xiaohongshu.com", "xhscdn.com"}
}

func (p *XiaohongshuPlugin) OnRequest(r *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
	return nil, nil
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
// Video notes: note web page URL with cover as preview (user clicks to get actual video via feed API).
func (p *XiaohongshuPlugin) emitUserPostedNote(note map[string]interface{}) bool {
	noteId, _ := note["note_id"].(string)
	if noteId == "" {
		return false
	}

	displayTitle, _ := note["display_title"].(string)
	noteType, _ := note["type"].(string)

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
		// Video note: emit with note web URL and cover as preview.
		// Actual video download URL will be captured when the user clicks into
		// the note detail page (feed API intercepted by OnResponse).
		noteUrl := fmt.Sprintf("https://www.xiaohongshu.com/explore/%s", noteId)

		urlSign := shared.Md5(noteUrl)
		if p.bridge.MediaIsMarked(urlSign) {
			return false
		}

		id, err := gonanoid.New()
		if err != nil {
			id = urlSign
		}

		res := shared.MediaInfo{
			Id:          id,
			Url:         noteUrl,
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
