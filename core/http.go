package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"res-downloader/core/shared"
	"strconv"
	"strings"

	"github.com/wailsapp/wails/v2/pkg/runtime"
	"github.com/xuri/excelize/v2"
)

type respData map[string]interface{}

type ResponseData struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data"`
}

type HttpServer struct{}

func initHttpServer() *HttpServer {
	if httpServerOnce == nil {
		httpServerOnce = &HttpServer{}
	}
	return httpServerOnce
}

func (h *HttpServer) run() {
	listener, err := net.Listen("tcp", globalConfig.Host+":"+globalConfig.Port)
	if err != nil {
		globalLogger.Err(err)
		log.Fatalf("Service cannot start: %v", err)
	}
	fmt.Println("Service started, listening http://" + globalConfig.Host + ":" + globalConfig.Port)
	if err1 := http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "127.0.0.1:"+globalConfig.Port && HandleApi(w, r) {

		} else {
			proxyOnce.Proxy.ServeHTTP(w, r) // 代理
		}
	})); err1 != nil {
		globalLogger.Err(err1)
		fmt.Printf("Service startup exception: %v", err1)
	}
}

func (h *HttpServer) downCert(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-x509-ca-data")
	w.Header().Set("Content-Disposition", "attachment;filename=res-downloader-public.crt")
	w.Header().Set("Content-Transfer-Encoding", "binary")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(appOnce.PublicCrt)))
	w.WriteHeader(http.StatusOK)
	io.Copy(w, io.NopCloser(bytes.NewReader(appOnce.PublicCrt)))
}

func (h *HttpServer) preview(w http.ResponseWriter, r *http.Request) {
	realURL := r.URL.Query().Get("url")
	if realURL == "" {
		http.Error(w, "Missing 'url' parameter", http.StatusBadRequest)
		return
	}
	realURL, _ = url.QueryUnescape(realURL)
	parsedURL, err := url.Parse(realURL)
	if err != nil {
		http.Error(w, "Invalid URL", http.StatusBadRequest)
		return
	}
	request, err := http.NewRequest("GET", parsedURL.String(), nil)
	if err != nil {
		http.Error(w, "Failed to fetch the resource", http.StatusInternalServerError)
		return
	}

	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		request.Header.Set("Range", rangeHeader)
	}

	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		http.Error(w, "Failed to fetch the resource", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	for k, v := range resp.Header {
		if strings.ToLower(k) == "access-control-allow-origin" {
			continue
		}
		for _, vv := range v {
			w.Header().Add(k, vv)
		}
	}
	w.WriteHeader(resp.StatusCode)

	_, err = io.Copy(w, resp.Body)
	if err != nil {
		http.Error(w, "Failed to serve the resource", http.StatusInternalServerError)
	}
	return
}

func (h *HttpServer) send(t string, data interface{}) {
	jsonData, err := json.Marshal(map[string]interface{}{
		"type": t,
		"data": data,
	})
	if err != nil {
		fmt.Println("Error converting map to JSON:", err)
		return
	}
	runtime.EventsEmit(appOnce.ctx, "event", string(jsonData))
}

func (h *HttpServer) writeJson(w http.ResponseWriter, data *ResponseData) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(200)
	err := json.NewEncoder(w).Encode(data)
	if err != nil {
		globalLogger.Err(err)
	}
}

func (h *HttpServer) error(w http.ResponseWriter, args ...interface{}) {
	message := "ok"
	var data interface{}

	if len(args) > 0 {
		message = args[0].(string)
	}
	if len(args) > 1 {
		data = args[1]
	}
	h.writeJson(w, h.buildResp(0, message, data))
}

func (h *HttpServer) success(w http.ResponseWriter, args ...interface{}) {
	message := "ok"
	var data interface{}

	if len(args) > 0 {
		data = args[0]
	}

	if len(args) > 1 {
		message = args[1].(string)
	}
	h.writeJson(w, h.buildResp(1, message, data))
}

func (h *HttpServer) buildResp(code int, message string, data interface{}) *ResponseData {
	return &ResponseData{
		Code:    code,
		Message: message,
		Data:    data,
	}
}

func (h *HttpServer) openDirectoryDialog(w http.ResponseWriter, r *http.Request) {
	folder, err := runtime.OpenDirectoryDialog(appOnce.ctx, runtime.OpenDialogOptions{
		DefaultDirectory: "",
		Title:            "Select a folder",
	})
	if err != nil {
		h.error(w, err.Error())
		return
	}
	h.success(w, respData{
		"folder": folder,
	})
}

func (h *HttpServer) openFileDialog(w http.ResponseWriter, r *http.Request) {
	filePath, err := runtime.OpenFileDialog(appOnce.ctx, runtime.OpenDialogOptions{
		Filters: []runtime.FileFilter{
			{
				DisplayName: "Videos (*.mov;*.mp4)",
				Pattern:     "*.mp4",
			},
		},
		Title: "Select a file",
	})
	if err != nil {
		h.error(w, err.Error())
		return
	}
	h.success(w, respData{
		"file": filePath,
	})
}

func (h *HttpServer) openFolder(w http.ResponseWriter, r *http.Request) {
	var data struct {
		FilePath string `json:"filePath"`
	}
	err := json.NewDecoder(r.Body).Decode(&data)
	if err == nil && data.FilePath == "" {
		return
	}

	err = shared.OpenFolder(data.FilePath)
	if err != nil {
		globalLogger.Err(err)
		h.error(w, err.Error())
		return
	}
	h.success(w)
	return
}

func (h *HttpServer) install(w http.ResponseWriter, r *http.Request) {
	if appOnce.isInstall() {
		h.success(w, respData{
			"isPass": systemOnce.Password == "",
		})
		return
	}

	out, err := appOnce.installCert()
	if err != nil {
		h.error(w, err.Error()+"\n"+out, respData{
			"isPass": systemOnce.Password == "",
		})
		return
	}

	h.success(w, respData{
		"isPass": systemOnce.Password == "",
	})
}

func (h *HttpServer) setSystemPassword(w http.ResponseWriter, r *http.Request) {
	var data struct {
		Password string `json:"password"`
		IsCache  bool   `json:"isCache"`
	}
	err := json.NewDecoder(r.Body).Decode(&data)
	if err != nil {
		h.error(w, err.Error())
		return
	}
	systemOnce.SetPassword(data.Password, data.IsCache)
	h.success(w)
}

func (h *HttpServer) openSystemProxy(w http.ResponseWriter, r *http.Request) {
	err := appOnce.OpenSystemProxy()
	if err != nil {
		h.error(w, err.Error(), respData{
			"value": appOnce.IsProxy,
		})
		return
	}
	h.success(w, respData{
		"value": appOnce.IsProxy,
	})
}

func (h *HttpServer) unsetSystemProxy(w http.ResponseWriter, r *http.Request) {
	err := appOnce.UnsetSystemProxy()
	if err != nil {
		h.error(w, err.Error(), respData{
			"value": appOnce.IsProxy,
		})
		return
	}
	h.success(w, respData{
		"value": appOnce.IsProxy,
	})
}

func (h *HttpServer) isProxy(w http.ResponseWriter, r *http.Request) {
	h.success(w, respData{
		"value": appOnce.IsProxy,
	})
}

func (h *HttpServer) appInfo(w http.ResponseWriter, r *http.Request) {
	h.success(w, appOnce)
}

func (h *HttpServer) getConfig(w http.ResponseWriter, r *http.Request) {
	h.success(w, globalConfig)
}

func (h *HttpServer) setConfig(w http.ResponseWriter, r *http.Request) {
	var data Config
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		h.error(w, err.Error())
		return
	}
	globalConfig.setConfig(data)
	h.success(w)
}

func (h *HttpServer) setType(w http.ResponseWriter, r *http.Request) {
	var data struct {
		Type string `json:"type"`
	}
	err := json.NewDecoder(r.Body).Decode(&data)
	if err == nil {
		if data.Type != "" {
			resourceOnce.setResType(strings.Split(data.Type, ","))
		} else {
			resourceOnce.setResType([]string{})
		}
	}

	h.success(w)
}

func (h *HttpServer) clear(w http.ResponseWriter, r *http.Request) {
	resourceOnce.clear()
	h.success(w)
}

func (h *HttpServer) delete(w http.ResponseWriter, r *http.Request) {
	var data struct {
		Sign []string `json:"sign"`
	}
	err := json.NewDecoder(r.Body).Decode(&data)
	if err == nil && len(data.Sign) > 0 {
		for _, v := range data.Sign {
			resourceOnce.delete(v)
		}
	}
	h.success(w)
}

func (h *HttpServer) download(w http.ResponseWriter, r *http.Request) {
	var data struct {
		shared.MediaInfo
		DecodeStr string `json:"decodeStr"`
	}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		h.error(w, err.Error())
		return
	}
	resourceOnce.download(data.MediaInfo, data.DecodeStr)
	h.success(w)
}

func (h *HttpServer) cancel(w http.ResponseWriter, r *http.Request) {
	var data struct {
		shared.MediaInfo
	}

	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		h.error(w, err.Error())
		return
	}

	err := resourceOnce.cancel(data.Id)
	if err != nil {
		h.error(w, err.Error())
		return
	}
	h.success(w)
}

func (h *HttpServer) wxFileDecode(w http.ResponseWriter, r *http.Request) {
	var data struct {
		shared.MediaInfo
		Filename  string `json:"filename"`
		DecodeStr string `json:"decodeStr"`
	}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		h.error(w, err.Error())
		return
	}
	savePath, err := resourceOnce.wxFileDecode(data.MediaInfo, data.Filename, data.DecodeStr)
	if err != nil {
		h.error(w, err.Error())
		return
	}
	h.success(w, respData{
		"save_path": savePath,
	})
}

func (h *HttpServer) kuaishouFetchList(w http.ResponseWriter, r *http.Request) {
	var data struct {
		UserId string `json:"userId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		h.error(w, err.Error())
		return
	}
	if data.UserId == "" {
		h.error(w, "userId is required")
		return
	}
	if !kuaishouPlugin.HasCookies() {
		h.error(w, "未检测到Cookie，请先开启代理并浏览快手页面")
		return
	}
	if err := kuaishouPlugin.FetchProfileVideos(data.UserId); err != nil {
		h.error(w, err.Error())
		return
	}
	h.success(w, respData{
		"message": "开始获取",
	})
}

func (h *HttpServer) kuaishouCancelFetch(w http.ResponseWriter, r *http.Request) {
	kuaishouPlugin.CancelFetch()
	h.success(w, respData{
		"message": "已取消",
	})
}

func (h *HttpServer) kuaishouFetchStatus(w http.ResponseWriter, r *http.Request) {
	h.success(w, respData{
		"isFetching": kuaishouPlugin.IsFetching(),
		"hasCookies": kuaishouPlugin.HasCookies(),
	})
}

func (h *HttpServer) batchExport(w http.ResponseWriter, r *http.Request) {
	var data struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		h.error(w, err.Error())
		return
	}
	fileName := filepath.Join(globalConfig.SaveDirectory, "res-downloader-"+shared.GetCurrentDateTimeFormatted()+".txt")
	err := os.WriteFile(fileName, []byte(data.Content), 0644)
	if err != nil {
		h.error(w, err.Error())
		return
	}

	_ = shared.OpenFolder(fileName)
	h.success(w, respData{
		"file_name": fileName,
	})
}

type exportDataItem struct {
	Description  string `json:"Description"`
	LikeCount    string `json:"likeCount"`
	FavCount     string `json:"favCount"`
	ForwardCount string `json:"forwardCount"`
	CommentCount string `json:"commentCount"`
	ViewCount    string `json:"viewCount"`
	Url          string `json:"Url"`
}

func (h *HttpServer) batchExportExcel(w http.ResponseWriter, r *http.Request) {
	var data struct {
		Items []exportDataItem `json:"items"`
	}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		h.error(w, err.Error())
		return
	}

	f := excelize.NewFile()
	defer f.Close()

	sheet := "Sheet1"

	// Header style
	headerStyle, _ := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Size: 12},
		Fill:      excelize.Fill{Type: "pattern", Color: []string{"#E8E8E8"}, Pattern: 1},
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center"},
	})

	headers := []string{"序号", "描述", "收藏", "点赞", "转发", "评论", "播放量", "链接"}
	colWidths := []float64{8, 60, 10, 10, 10, 10, 12, 80}
	for i, header := range headers {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		f.SetCellValue(sheet, cell, header)
		f.SetCellStyle(sheet, cell, cell, headerStyle)
		col, _ := excelize.ColumnNumberToName(i + 1)
		f.SetColWidth(sheet, col, col, colWidths[i])
	}

	for idx, item := range data.Items {
		row := idx + 2
		f.SetCellValue(sheet, cellName(1, row), idx+1)
		f.SetCellValue(sheet, cellName(2, row), item.Description)
		setIntOrStr(f, sheet, 3, row, item.LikeCount)
		setIntOrStr(f, sheet, 4, row, item.FavCount)
		setIntOrStr(f, sheet, 5, row, item.ForwardCount)
		setIntOrStr(f, sheet, 6, row, item.CommentCount)
		setIntOrStr(f, sheet, 7, row, item.ViewCount)
		f.SetCellValue(sheet, cellName(8, row), item.Url)
	}

	fileName := filepath.Join(globalConfig.SaveDirectory, "res-downloader-"+shared.GetCurrentDateTimeFormatted()+".xlsx")
	if err := f.SaveAs(fileName); err != nil {
		h.error(w, err.Error())
		return
	}

	_ = shared.OpenFolder(fileName)
	h.success(w, respData{
		"file_name": fileName,
	})
}

func cellName(col, row int) string {
	name, _ := excelize.CoordinatesToCellName(col, row)
	return name
}

func setIntOrStr(f *excelize.File, sheet string, col, row int, val string) {
	if v, err := strconv.ParseInt(val, 10, 64); err == nil {
		f.SetCellValue(sheet, cellName(col, row), v)
	} else {
		f.SetCellValue(sheet, cellName(col, row), val)
	}
}

func (h *HttpServer) bilibiliFetchList(w http.ResponseWriter, r *http.Request) {
	var data struct {
		Mid string `json:"mid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		h.error(w, err.Error())
		return
	}
	if data.Mid == "" {
		h.error(w, "mid is required")
		return
	}
	if !bilibiliPlugin.HasCookies() {
		h.error(w, "未检测到Cookie，请先开启代理并浏览B站页面")
		return
	}
	if err := bilibiliPlugin.FetchProfileVideos(data.Mid); err != nil {
		h.error(w, err.Error())
		return
	}
	h.success(w, respData{
		"message": "开始获取",
	})
}

func (h *HttpServer) bilibiliCancelFetch(w http.ResponseWriter, r *http.Request) {
	bilibiliPlugin.CancelFetch()
	h.success(w, respData{
		"message": "已取消",
	})
}

func (h *HttpServer) bilibiliFetchStatus(w http.ResponseWriter, r *http.Request) {
	h.success(w, respData{
		"isFetching": bilibiliPlugin.IsFetching(),
		"hasCookies": bilibiliPlugin.HasCookies(),
	})
}
