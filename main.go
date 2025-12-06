package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/dgraph-io/badger/v4"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// ================== Config Struct ==================
type Config struct {
	EmbyServer    string    `yaml:"emby_server"`
	LogLevel      string    `yaml:"log_level"`
	EmbyApiKey    string    `yaml:"emby_api_key"`
	Hide          []string  `yaml:"hide"`
	Library       []Library `yaml:"library"`
	VirtualInsert string    `yaml:"virtual_insert"` // prepend | append | index:<n> | after_type:<type>
	ViewsOrder    []ViewOrderItem `yaml:"views_order"` // 全序控制：优先于 VirtualInsert
}

type ViewOrderItem struct {
	Virtual  string `yaml:"virtual,omitempty"`   // 虚拟库名称（config.Library[].Name）
	RealType string `yaml:"real_type,omitempty"` // 真实库 CollectionType（movies/tvshows/music/photos/boxsets/...）
	RealName string `yaml:"real_name,omitempty"` // 真实库显示名
	RealID   string `yaml:"real_id,omitempty"`   // 真实库 Id
}

type Library struct {
	Name         string `yaml:"name"`
	ResourceID   string `yaml:"resource_id"`
	ResourceType string `yaml:"resource_type"`
	Image        string `yaml:"image"`
}

func (l *Library) NeedRecursive() bool {
	return l.ResourceType != "collection"
}

// 返回参数名
func (l *Library) GetParamKey() string {
	switch l.ResourceType {
	case "collection":
		return "ParentId"
	case "tag":
		return "TagIds"
	case "genre":
		return "GenreIds"
	case "studio":
		return "StudioIds"
	case "person":
		return "PersonIds"
	default:
		return ""
	}
}

var config Config
var libraryMap = map[string]Library{}

// 可选 /emby 前缀 + 可选 Users 段的正则
var (
	pathPrefix        = `(?:/emby)?`
	hookViewsRe       = regexp.MustCompile(pathPrefix + `/Users/[^/]+/Views$`)
	hookLatestRe      = regexp.MustCompile(pathPrefix + `/(?:Users/[^/]+/)?Items/Latest$`)
	hookDetailsRe     = regexp.MustCompile(pathPrefix + `/(?:Users/[^/]+/)?Items$`)
	hookDetailIntroRe = regexp.MustCompile(pathPrefix + `/(?:Users/[^/]+/)?Items/\d+$`)
	hookImageRe       = regexp.MustCompile(pathPrefix + `/(?:Users/[^/]+/)?Items/\d+/Images/(?:P|p)rimary$`)
)

type ResponseHook struct {
	Pattern *regexp.Regexp
	Handler func(*http.Response) error
}

var responseHooks = []ResponseHook{
	{hookViewsRe, hookViews},
	{hookLatestRe, hookLatest},
	{hookDetailsRe, hookDetails},
	{hookDetailIntroRe, hookDetailIntro},
	{hookImageRe, hookImage},
}

var badgerDB *badger.DB

// ================== Utility Functions ==================
func LoadConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var cfg Config
	decoder := yaml.NewDecoder(f)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func HashNameToID(name string) string {
	h := fnv.New32a()
	h.Write([]byte(name))
	return strconv.FormatUint(uint64(h.Sum32()), 10)
}

// 获取 userId（支持 /emby/Users/{uid}/... 或 /emby/Items?...&UserId=xxx）
func getUserId(req *http.Request) string {
	path := req.URL.Path
	parts := strings.Split(path, "/")
	userId := ""

	if len(parts) > 1 && parts[1] == "emby" {
		if len(parts) > 3 && parts[2] == "Users" {
			userId = parts[3]
		}
	} else {
		if len(parts) > 2 && parts[1] == "Users" {
			userId = parts[2]
		}
	}
	if userId == "" {
		if v := req.URL.Query().Get("UserId"); v != "" {
			userId = v
		}
	}
	return userId
}

// 拼接 Emby API URL
func embyURL(path string, userId string) string {
	return config.EmbyServer + strings.Replace(path, "{userId}", userId, 1)
}

// 通用 GET 请求并解析 JSON（带超时）
func doGetJSON(
	baseURL string,
	query url.Values,
	headers http.Header,
	cookies []*http.Cookie,
) (map[string]interface{}, error) {
	client := &http.Client{Timeout: 12 * time.Second}
	req, err := http.NewRequest("GET", baseURL, nil)
	if err != nil {
		return nil, err
	}
	if query != nil {
		req.URL.RawQuery = query.Encode()
	}
	if headers != nil {
		for k, v := range headers {
			for _, vv := range v {
				req.Header.Add(k, vv)
			}
		}
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	return data, nil
}

// 仅通过 Header 透传 X-Emby-*（query 不塞）
func setXEmbyParams(_ url.Values, originalQuery url.Values, headers http.Header, originalHeaders http.Header) {
	xEmbyKeys := []string{
		"X-Emby-Client",
		"X-Emby-Device-Name",
		"X-Emby-Device-Id",
		"X-Emby-Client-Version",
		"X-Emby-Token",
		"X-Emby-Language",
		"X-Emby-Authorization",
	}
	_ = originalQuery
	for _, key := range xEmbyKeys {
		if headerVal := originalHeaders.Get(key); headerVal != "" {
			headers.Set(key, headerVal)
		}
	}
}

func getAllCollections(boxId string, orignalReq *http.Request) []map[string]interface{} {
	userId := getUserId(orignalReq)

	query := url.Values{}
	query.Set("ParentId", boxId)

	headers := http.Header{}
	setXEmbyParams(query, orignalReq.URL.Query(), headers, orignalReq.Header)

	headers.Set("Accept-Language", orignalReq.Header.Get("Accept-Language"))
	headers.Set("User-Agent", orignalReq.Header.Get("User-Agent"))
	headers.Set("accept", "application/json")

	cookies := orignalReq.Cookies()

	url := embyURL("/emby/Users/{userId}/Items", userId)
	data, err := doGetJSON(url, query, headers, cookies)
	if err != nil {
		return nil
	}
	var collections []map[string]interface{}
	items, _ := data["Items"].([]interface{})
	for _, item := range items {
		collections = append(collections, item.(map[string]interface{}))
	}
	return collections
}

func getFirstBoxset(orignalReq *http.Request) map[string]interface{} {
	userId := getUserId(orignalReq)

	query := url.Values{}

	headers := http.Header{}
	setXEmbyParams(query, orignalReq.URL.Query(), headers, orignalReq.Header)

	headers.Set("Accept-Language", orignalReq.Header.Get("Accept-Language"))
	headers.Set("User-Agent", orignalReq.Header.Get("User-Agent"))
	headers.Set("accept", "application/json")

	cookies := orignalReq.Cookies()

	url := embyURL("/emby/Users/{userId}/Views", userId)
	data, err := doGetJSON(url, query, headers, cookies)
	if err != nil {
		return nil
	}
	items, _ := data["Items"].([]interface{})
	var boxsets map[string]interface{}
	for _, item := range items {
		if item.(map[string]interface{})["CollectionType"] == "boxsets" {
			boxsets = item.(map[string]interface{})
			break
		}
	}
	if boxsets == nil {
		return nil
	}
	return boxsets
}

func ensureCollectionExist(id string, orignalReq *http.Request) bool {
	boxsets := getFirstBoxset(orignalReq)
	if boxsets == nil {
		log.Info("boxsets is nil")
		return false
	}
	collectionId := boxsets["Id"].(string)
	collections := getAllCollections(collectionId, orignalReq)
	if len(collections) == 0 {
		log.Info("collections is empty")
		return false
	}
	for _, collection := range collections {
		if collection["Id"].(string) == id {
			log.Info("collection exist", id)
			return true
		}
	}
	log.Info("collection not exist", id)
	return false
}

func getCollectionDataWithApi(lib Library, apiKey string) map[string]interface{} {
	query := url.Values{}
	if lib.GetParamKey() != "" && lib.ResourceID != "" {
		query.Set(lib.GetParamKey(), lib.ResourceID)
	}
	query.Set("ImageTypeLimit", "1")
	if lib.NeedRecursive() {
		query.Set("Recursive", "true")
	}
	query.Set("Fields", "BasicSyncInfo,CanDelete,CanDownload,PrimaryImageAspectRatio,ProductionYear,Status,EndDate")
	query.Set("EnableTotalRecordCount", "true")
	query.Set("api_key", apiKey) // 小写

	url := fmt.Sprintf("%s/emby/Items", config.EmbyServer)
	headers := http.Header{}
	headers.Set("accept", "application/json")
	data, err := doGetJSON(url, query, headers, nil)
	if err != nil {
		return nil
	}
	return data
}

// getItems：按库 + 请求参数聚合
func getItems(lib Library, orignalReq *http.Request, extQuery url.Values) map[string]interface{} {
	orignalQuery := orignalReq.URL.Query()
	query := url.Values{} // 避免污染原始 query

	if lib.GetParamKey() != "" && lib.ResourceID != "" {
		query.Set(lib.GetParamKey(), lib.ResourceID)
	}
	log.Debug("getItems query ", query)
	log.Debug("getItems orignalReq header ", orignalReq.Header)
	log.Debug("getItems orignalReq url ", orignalReq.URL)
	log.Debug("getItems orignalReq url path ", orignalReq.URL.Path)
	log.Debug("getItems orignalReq url query ", orignalReq.URL.Query())
	log.Debug("getItems extQuery ", extQuery)

	// 过滤掉合集/播放列表等，保留常见媒体类型
	query.Set("IncludeItemTypes", "Movie,Series,Video,Game,MusicAlbum,Episode")
	if v := orignalQuery.Get("ImageTypeLimit"); v != "" {
		query.Set("ImageTypeLimit", v)
	} else {
		query.Set("ImageTypeLimit", "1")
	}
	if v := orignalQuery.Get("Fields"); v != "" {
		query.Set("Fields", v)
	}
	if v := orignalQuery.Get("EnableTotalRecordCount"); v != "" {
		query.Set("EnableTotalRecordCount", v)
	}
	if v := orignalQuery.Get("Filters"); v != "" {
		query.Set("Filters", v)
	}
	if lib.NeedRecursive() {
		query.Set("Recursive", "true")
	}
	if extQuery != nil {
		for k, v := range extQuery {
			query.Set(k, v[0])
		}
	} else {
		if v := orignalQuery.Get("SortBy"); v != "" {
			query.Set("SortBy", v)
		}
		if v := orignalQuery.Get("SortOrder"); v != "" {
			query.Set("SortOrder", v)
		}
	}

	headers := http.Header{}
	setXEmbyParams(query, orignalReq.URL.Query(), headers, orignalReq.Header)
	log.Debug("getItems query after setXEmbyParams ", query)

	headers.Set("Accept-Language", orignalReq.Header.Get("Accept-Language"))
	headers.Set("User-Agent", orignalReq.Header.Get("User-Agent"))
	headers.Set("accept", "application/json")

	cookies := orignalReq.Cookies()

	userId := getUserId(orignalReq)
	url := embyURL("/emby/Users/{userId}/Items", userId)
	data, err := doGetJSON(url, query, headers, cookies)
	if err != nil {
		return nil
	}
	items, _ := data["Items"].([]interface{})
	log.Debug("getCollectionData data count", len(items))
	return data
}

// 公共：按响应的 Content-Encoding 进行重新编码
func encodeBodyByContentEncoding(body []byte, encoding string) ([]byte, error) {
	var buf bytes.Buffer
	switch encoding {
	case "gzip":
		gz := gzip.NewWriter(&buf)
		if _, err := gz.Write(body); err != nil {
			return nil, err
		}
		gz.Close()
		return buf.Bytes(), nil
	case "deflate":
		df, err := flate.NewWriter(&buf, flate.DefaultCompression)
		if err != nil {
			return nil, err
		}
		if _, err := df.Write(body); err != nil {
			return nil, err
		}
		df.Close()
		return buf.Bytes(), nil
	case "br":
		br := brotli.NewWriter(&buf)
		if _, err := br.Write(body); err != nil {
			return nil, err
		}
		br.Close()
		return buf.Bytes(), nil
	default:
		return body, nil // 不压缩
	}
}

// 公共：替换响应体（先耗尽并关闭上游 body，避免连接泄露）
func replaceBody(resp *http.Response, bodyBytes []byte) error {
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	encoding := resp.Header.Get("Content-Encoding")
	encodedBody, err := encodeBodyByContentEncoding(bodyBytes, encoding)
	if err != nil {
		return err
	}
	resp.Body = io.NopCloser(bytes.NewReader(encodedBody))
	resp.ContentLength = int64(len(encodedBody))
	resp.Header.Set("Content-Length", strconv.Itoa(len(encodedBody)))
	return nil
}

// =============== image hook ===============
func hookImage(resp *http.Response) error {
	log.Debug("hookImage")
	// 先取 ?tag / ?Tag；没有就从路径中提取 Items/{id}/Images/Primary 的 {id}
	tag := resp.Request.URL.Query().Get("tag")
	if tag == "" {
		tag = resp.Request.URL.Query().Get("Tag")
	}
	if tag == "" {
		components := strings.Split(resp.Request.URL.Path, "/")
		// /emby/Items/{id}/Images/Primary 或 /emby/Users/{uid}/Items/{id}/Images/Primary
		// 倒数第三段是 {id}
		if len(components) >= 3 {
			tag = components[len(components)-3]
		}
	}
	log.Debug("hookImage tag ", tag)
	if tag == "" {
		return nil
	}
	lib, ok := libraryMap[tag]
	if !ok {
		log.Warn("hookImage tag not found ", tag)
		return nil
	}
	log.Debug("hookImage tag", tag)
	var image []byte
	if lib.Image != "" {
		userImage, err := os.ReadFile(lib.Image)
		if err != nil {
			return err
		}
		image = userImage
		resp.Header.Set("Cache-Control", "public, max-age=86400")
	} else {
		path := fmt.Sprintf("images/%s.png", lib.Name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			placeholder, err := os.ReadFile("assets/placeholder.png")
			if err != nil {
				return err
			}
			image = placeholder
		} else {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			image = b
			resp.Header.Set("Cache-Control", "public, max-age=86400")
		}
	}
	contentType := http.DetectContentType(image)
	if err := replaceBody(resp, image); err != nil {
		return err
	}
	encoding := resp.Header.Get("Content-Encoding")
	if encoding == "" {
		resp.Header.Del("Content-Encoding")
	} else {
		resp.Header.Set("Content-Encoding", encoding)
	}
	resp.Header.Set("Content-Type", contentType)
	resp.StatusCode = 200
	resp.Status = "200 OK"
	return nil
}

// =============== detail intro hook ===============
func hookDetailIntro(resp *http.Response) error {
	template := `{
    "Name": "Sample Library",
    "ServerId": "",
    "Id": "1241",
    "Guid": "470c3d1e3b5e4a0287ad485a5cf67207",
    "Etag": "8281abb37d32a2b95db7e5a5df4407a4",
    "DateCreated": "2025-04-19T09:07:17.0000000Z",
    "CanDelete": false,
    "CanDownload": false,
    "PresentationUniqueKey": "470c3d1e3b5e4a0287ad485a5cf67207",
    "SupportsSync": true,
    "SortName": "Sample Library",
    "ForcedSortName": "Sample Library",
    "ExternalUrls": [],
    "Taglines": [],
    "RemoteTrailers": [],
    "ProviderIds": {},
    "IsFolder": true,
    "ParentId": "1",
    "Type": "UserView",
    "UserData": {
        "PlaybackPositionTicks": 0,
        "IsFavorite": false,
        "Played": false
    },
    "ChildCount": 1,
    "DisplayPreferencesId": "470c3d1e3b5e4a0287ad485a5cf67207",
    "PrimaryImageAspectRatio": 1.7777777777777777,
    "CollectionType": "tvshows",
    "ImageTags": {
        "Primary": "79219cbf328f6dfc6e2b3ad599233d34"
    },
    "BackdropImageTags": [],
    "LockedFields": [],
    "LockData": false,
    "Subviews": [
        "series",
        "studios",
        "genres",
        "episodes",
        "series",
        "folders"
    ]
}`
	components := strings.Split(resp.Request.URL.Path, "/")
	id := components[len(components)-1]
	lib, ok := libraryMap[id]
	if !ok {
		return nil
	}
	log.Debug("hookDetailIntro id", id)
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(template), &data); err != nil {
		return err
	}
	// 用库名和 hash id 替换
	data["Name"] = lib.Name
	data["Id"] = id
	data["ImageTags"] = map[string]string{
		"Primary": id,
	}
	bodyBytes, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if err := replaceBody(resp, bodyBytes); err != nil {
		return err
	}
	encoding := resp.Header.Get("Content-Encoding")
	if encoding == "" {
		resp.Header.Del("Content-Encoding")
	} else {
		resp.Header.Set("Content-Encoding", encoding)
	}
	resp.Header.Set("Content-Type", "application/json")
	resp.StatusCode = 200
	resp.Status = "200 OK"
	return nil
}

// =============== details hook（列表页） ===============
func hookDetails(resp *http.Response) error {
	log.Debug("hookDetails")
	parentId := resp.Request.URL.Query().Get("ParentId")
	lib, ok := libraryMap[parentId]
	if !ok {
		// 某些客户端用 Items 直接取视图，兜底走 hookViews
		hasId := false
		for key := range resp.Request.URL.Query() {
			if key == "UserId" {
				continue
			}
			if strings.HasSuffix(key, "Id") {
				hasId = true
				break
			}
		}
		if !hasId {
			return hookViews(resp)
		}
		return nil
	}
	bodyText := getItems(lib, resp.Request, nil)
	bodyBytes, err := json.Marshal(bodyText)
	if err != nil {
		return err
	}
	if err := replaceBody(resp, bodyBytes); err != nil {
		return err
	}
	encoding := resp.Header.Get("Content-Encoding")
	if encoding == "" {
		resp.Header.Del("Content-Encoding")
	} else {
		resp.Header.Set("Content-Encoding", encoding)
	}
	resp.Header.Set("Content-Type", "application/json")
	return nil
}

// =============== latest hook（最近添加） ===============
func hookLatest(resp *http.Response) error {
	log.Debug("hookLatest")
	start := time.Now()
	parentId := resp.Request.URL.Query().Get("ParentId")
	lib, ok := libraryMap[parentId]
	if !ok {
		return nil
	}
	query := url.Values{}
	query.Set("SortBy", "DateLastContentAdded,DateCreated,SortName")
	query.Set("SortOrder", "Descending")
	if v := resp.Request.URL.Query().Get("Limit"); v != "" {
		query.Set("Limit", v)
	}
	query.Set("IsPlayed", "false")
	if lib.NeedRecursive() {
		query.Set("Recursive", "true")
	}
	log.Debug("before getCollectionData")
	getDataStart := time.Now()
	items := getItems(lib, resp.Request, query)["Items"].([]interface{})
	log.Debugf("getCollectionData done, cost: %v, items: %d", time.Since(getDataStart), len(items))
	marshalStart := time.Now()
	bodyBytes, err := json.Marshal(items)
	log.Debugf("json.Marshal done, cost: %v", time.Since(marshalStart))
	if err != nil {
		return err
	}
	if err := replaceBody(resp, bodyBytes); err != nil {
		return err
	}
	encoding := resp.Header.Get("Content-Encoding")
	if encoding == "" {
		resp.Header.Del("Content-Encoding")
	} else {
		resp.Header.Set("Content-Encoding", encoding)
	}
	log.Debugf("hookLatest total cost: %v", time.Since(start))
	return nil
}

// =============== views hook（核心：视图页重排） ===============
func hookViews(resp *http.Response) error {
	template := `{
		"BackdropImageTags": [],
		"CanDelete": false,
		"CanDownload": false,
		"ChildCount": 1,
		"CollectionType": "tvshows",
		"DateCreated": "2025-04-19T09:07:17.0000000Z",
		"DisplayPreferencesId": "470c3d1e3b5e4a0287ad485a5cf67207",
		"Etag": "8281abb37d32a2b95db7e5a5df4407a4",
		"ExternalUrls": [],
		"ForcedSortName": "Sample Library",
		"Guid": "470c3d1e3b5e4a0287ad485a5cf67207",
		"Id": "1241",
		"ImageTags": {
			"Primary": "79219cbf328f6dfc6e2b3ad599233d34"
		},
		"IsFolder": true,
		"LockData": false,
		"LockedFields": [],
		"Name": "Sample Library",
		"ParentId": "1",
		"PresentationUniqueKey": "470c3d1e3b5e4a0287ad485a5cf67207",
		"PrimaryImageAspectRatio": 1.7777777777777777,
		"ProviderIds": {},
		"RemoteTrailers": [],
		"ServerId": "",
		"SortName": "Sample Library",
		"Taglines": [],
		"Type": "UserView",
		"UserData": {
			"IsFavorite": false,
			"PlaybackPositionTicks": 0,
			"Played": false
		}
	}`
	log.Debug("hookViews")
	var bodyBytes []byte
	var err error

	// 读取上游 body（按编码）
	switch resp.Header.Get("Content-Encoding") {
	case "br":
		br := brotli.NewReader(resp.Body)
		bodyBytes, err = io.ReadAll(br)
		resp.Body.Close()
	case "deflate":
		df := flate.NewReader(resp.Body)
		bodyBytes, err = io.ReadAll(df)
		resp.Body.Close()
	case "gzip":
		gz, e := gzip.NewReader(resp.Body)
		if e != nil {
			log.Warn("gzip.NewReader error", e)
		}
		bodyBytes, err = io.ReadAll(gz)
		if err != nil {
			log.Warn("io.ReadAll error", err)
		}
		resp.Body.Close()
	default:
		bodyBytes, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if err != nil {
		return err
	}

	var data map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &data); err != nil {
		log.Warn("json.Unmarshal error", err)
		// 解析失败则透传
		resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		return nil
	}
	rawItems, _ := data["Items"].([]interface{})
	if len(rawItems) == 0 {
		return nil
	}
	typedItems := make([]map[string]interface{}, 0, len(rawItems))
	for _, it := range rawItems {
		typedItems = append(typedItems, it.(map[string]interface{}))
	}
	serverId := typedItems[0]["ServerId"].(string)
	log.Debug("Items count ", len(typedItems))

	// 生成“虚拟库”条目
	var newItems []map[string]interface{}
	for _, lib := range config.Library {
		var item map[string]interface{}
		if err := json.Unmarshal([]byte(template), &item); err != nil {
			continue
		}
		item["Name"] = lib.Name
		item["SortName"] = lib.Name
		item["ForcedSortName"] = lib.Name
		item["Id"] = HashNameToID(lib.Name)
		item["ImageTags"] = map[string]string{
			"Primary": HashNameToID(lib.Name),
		}
		item["ServerId"] = serverId
		newItems = append(newItems, item)
	}

	// 根据 Hide 过滤真实库
	if len(config.Hide) > 0 {
		if slices.Contains(config.Hide, "all") {
			typedItems = []map[string]interface{}{}
		} else {
			oldItems := []map[string]interface{}{}
			for _, item := range typedItems {
				ct, _ := item["CollectionType"].(string)
				if len(config.Hide) > 0 && slices.Contains(config.Hide, ct) {
					continue
				}
				oldItems = append(oldItems, item)
			}
			typedItems = oldItems
		}
	}

	// === 关键：全序控制 / 回落策略 ===
	if len(config.ViewsOrder) > 0 {
		typedItems = reorderViews(typedItems, newItems, config.ViewsOrder)
	} else {
		idx := computeInsertIndex(config.VirtualInsert, typedItems)
		typedItems = insertMany(typedItems, idx, newItems)
	}

	// 写回
	data["Items"] = typedItems
	newBody, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if err := replaceBody(resp, newBody); err != nil {
		return err
	}
	encoding := resp.Header.Get("Content-Encoding")
	if encoding == "" {
		resp.Header.Del("Content-Encoding")
	} else {
		resp.Header.Set("Content-Encoding", encoding)
	}
	return nil
}

// =============== 视图重排工具（全序） ===============
func reorderViews(real, virt []map[string]interface{}, orders []ViewOrderItem) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(real)+len(virt))
	usedReal := make(map[string]bool) // key: real Id
	usedVirt := make(map[string]bool) // key: lower(name)

	norm := func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

	appendRealType := func(t string) {
		t = norm(t)
		for _, r := range real {
			id := fmt.Sprint(r["Id"])
			if usedReal[id] {
				continue
			}
			if ct, ok := r["CollectionType"].(string); ok && norm(ct) == t {
				out = append(out, r)
				usedReal[id] = true
			}
		}
	}
	appendRealName := func(name string) {
		want := norm(name)
		for _, r := range real {
			id := fmt.Sprint(r["Id"])
			if usedReal[id] {
				continue
			}
			if rn, ok := r["Name"].(string); ok && norm(rn) == want {
				out = append(out, r)
				usedReal[id] = true
				break
			}
		}
	}
	appendRealID := func(want string) {
		want = strings.TrimSpace(want)
		for _, r := range real {
			id := fmt.Sprint(r["Id"])
			if usedReal[id] {
				continue
			}
			if id == want {
				out = append(out, r)
				usedReal[id] = true
				break
			}
		}
	}
	appendVirtualName := func(name string) {
		want := norm(name)
		if usedVirt[want] {
			return
		}
		for _, v := range virt {
			vn, _ := v["Name"].(string)
			if norm(vn) == want {
				out = append(out, v)
				usedVirt[want] = true
				return
			}
		}
	}

	// 按 views_order 顺序逐项放置
	for _, it := range orders {
		switch {
		case it.Virtual != "":
			appendVirtualName(it.Virtual)
		case it.RealType != "":
			appendRealType(it.RealType)
		case it.RealName != "":
			appendRealName(it.RealName)
		case it.RealID != "":
			appendRealID(it.RealID)
		}
	}

	// 追加剩余虚拟库（保持 newItems 原顺序）
	for _, v := range virt {
		vn, _ := v["Name"].(string)
		if !usedVirt[norm(vn)] {
			out = append(out, v)
		}
	}
	// 追加剩余真实库（保持 Emby 原顺序）
	for _, r := range real {
		id := fmt.Sprint(r["Id"])
		if !usedReal[id] {
			out = append(out, r)
		}
	}
	return out
}

// 兼容旧策略：插入工具
func insertMany(dst []map[string]interface{}, idx int, src []map[string]interface{}) []map[string]interface{} {
	if idx <= 0 {
		return append(src, dst...)
	}
	if idx >= len(dst) {
		return append(dst, src...)
	}
	out := make([]map[string]interface{}, 0, len(dst)+len(src))
	out = append(out, dst[:idx]...)
	out = append(out, src...)
	out = append(out, dst[idx:]...)
	return out
}

func computeInsertIndex(policy string, real []map[string]interface{}) int {
	p := strings.TrimSpace(policy)
	if p == "" || p == "prepend" {
		return 0
	}
	if p == "append" {
		return len(real)
	}
	if strings.HasPrefix(p, "index:") {
		nStr := strings.TrimPrefix(p, "index:")
		if n, err := strconv.Atoi(nStr); err == nil {
			if n < 0 {
				n = 0
			}
			if n > len(real) {
				n = len(real)
			}
			return n
		}
		return 0
	}
	if strings.HasPrefix(p, "after_type:") {
		t := strings.TrimPrefix(p, "after_type:")
		for i, it := range real {
			if ct, ok := it["CollectionType"].(string); ok && strings.EqualFold(ct, t) {
				return i + 1
			}
		}
		return len(real)
	}
	return 0
}

// =============== modifyResponse 入口 ===============
func modifyResponse(resp *http.Response) error {
	for _, hook := range responseHooks {
		if hook.Pattern.MatchString(resp.Request.URL.Path) {
			log.Debug("matched", resp.Request.URL.Path)
			log.Debug("hook", hook.Pattern.String())
			log.Debug("hook start", resp.Request.URL.Path)
			hookStart := time.Now()
			err := hook.Handler(resp)
			log.Debugf("hook %s cost: %v", resp.Request.URL.Path, time.Since(hookStart))
			return err
		}
	}
	return nil
}

func getImage(lib *Library) error {
	alreadyGenerated := false
	err := badgerDB.View(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(lib.Name))
		if err != nil {
			if err != badger.ErrKeyNotFound {
				log.Warn("badgerDB.View error", err)
			}
			return nil
		}
		return item.Value(func(val []byte) error {
			if string(val) == "1" {
				log.Debug("badgerDB.View item", lib.Name, "already generated")
				alreadyGenerated = true
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	fileName := fmt.Sprintf("images/%s.png", lib.Name)
	fileExist, err := os.Stat(fileName)
	if alreadyGenerated && err == nil && fileExist.Size() > 0 {
		return nil
	}
	log.Debug("cover gen start", lib.Name)

	data := getCollectionDataWithApi(*lib, config.EmbyApiKey)
	if data == nil {
		log.Debug("no data for cover gen", lib.Name)
		return nil
	}
	items, _ := data["Items"].([]interface{})
	itemCount := len(items)
	if itemCount == 0 {
		log.Debug("no available image", lib.Name)
		return nil // 没有可用图片
	}

	var selected []interface{}
	if itemCount <= 9 {
		selected = items
	} else {
		rand.Shuffle(itemCount, func(i, j int) { items[i], items[j] = items[j], items[i] })
		selected = items[:9]
	}

	httpClient := &http.Client{Timeout: 12 * time.Second}

	for i, itemRaw := range selected {
		item := itemRaw.(map[string]interface{})
		imageTags, ok := item["ImageTags"].(map[string]interface{})
		if !ok {
			continue
		}
		imageId, ok := imageTags["Primary"].(string)
		if !ok {
			continue
		}
		itemId, ok := item["Id"].(string)
		if !ok {
			continue
		}
		imageUrl := fmt.Sprintf("%s/emby/Items/%s/Images/Primary?maxHeight=600&maxWidth=400&tag=%s&quality=90&api_key=%s",
			config.EmbyServer, itemId, imageId, url.QueryEscape(config.EmbyApiKey))
		req, _ := http.NewRequest("GET", imageUrl, nil)
		resp, err := httpClient.Do(req)
		if err != nil {
			return err
		}
		imageBytes, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}
		_ = os.MkdirAll(fmt.Sprintf("images/%s", lib.Name), 0755)
		if err := os.WriteFile(fmt.Sprintf("images/%s/%d.jpg", lib.Name, i+1), imageBytes, 0644); err != nil {
			return err
		}
	}
	// 生成拼图
	cmd := exec.Command("uv", "run", "python", "cover_gen.py", lib.Name)
	cmd.Dir = "."
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}

	_ = badgerDB.Update(func(txn *badger.Txn) error {
		return txn.Set([]byte(lib.Name), []byte("1"))
	})
	return nil
}

func main() {
	cfg, err := LoadConfig("config.yaml")
	if err != nil {
		log.Warn("LoadConfig error", err)
		return
	}
	config = *cfg

	// 设置日志级别
	switch strings.ToLower(config.LogLevel) {
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "warn":
		log.SetLevel(log.WarnLevel)
	case "error":
		log.SetLevel(log.ErrorLevel)
	default:
		log.SetLevel(log.InfoLevel)
	}
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp: true,
	})

	// 初始化 Badger
	badgerDB, err = badger.Open(badger.DefaultOptions("images/badger_db").WithLogger(nil))
	if err != nil {
		log.Warn("badger open error", err)
		return
	}
	defer badgerDB.Close()

	for _, lib := range config.Library {
		libraryMap[HashNameToID(lib.Name)] = lib
	}

	target, err := url.Parse(config.EmbyServer)
	if err != nil {
		log.Warn("url.Parse error", err)
		return
	}

	proxy := httputil.NewSingleHostReverseProxy(target)

	// 上游 Transport：禁用透明解压，由我们手动按 Content-Encoding 处理
	proxy.Transport = &http.Transport{
		Proxy:              http.ProxyFromEnvironment,
		DisableCompression: true,
	}

	// 修改 Director 保证 Host 头正确，并补充转发头
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = target.Host

		// 获取客户端IP
		clientIP, _, _ := net.SplitHostPort(req.RemoteAddr)
		if clientIP != "" {
			// X-Forwarded-For
			prior := req.Header.Get("X-Forwarded-For")
			if prior != "" {
				req.Header.Set("X-Forwarded-For", prior+", "+clientIP)
			} else {
				req.Header.Set("X-Forwarded-For", clientIP)
			}
			// X-Real-IP
			req.Header.Set("X-Real-IP", clientIP)
		}

		// X-Forwarded-Proto / Protocol
		scheme := "http"
		if req.TLS != nil {
			scheme = "https"
		}
		req.Header.Set("X-Forwarded-Proto", scheme)
		req.Header.Set("X-Forwarded-Protocol", scheme)
	}

	// 修改响应，处理重定向 / 注入视图
	proxy.ModifyResponse = func(resp *http.Response) error {
		return modifyResponse(resp)
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeHTTP(w, r)
	})

	// 异步获取封面图
	for _, lib := range config.Library {
		libCopy := lib // 防止闭包变量问题
		go func(l Library) {
			if err := getImage(&l); err != nil {
				log.Warn("getImage error", err)
			}
		}(libCopy)
	}

	log.Info("emby-virtual-lib listen on :8000")
	if err := http.ListenAndServe(":8000", nil); err != nil {
		log.Fatal(err)
	}
}
