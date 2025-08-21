package api

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
    "encoding/json"
    "io"
    "strings"

	"github.com/alexmullins/zip"
	"github.com/labstack/echo/v4"
	"github.com/monaco-io/request"

	"sealdice-core/dice"
)

type Response map[string]interface{}

func Success(c *echo.Context, res Response) error {
	res["result"] = true
	return (*c).JSON(http.StatusOK, res)
}

func Error(c *echo.Context, errMsg string, res Response) error {
	res["result"] = false
	res["err"] = errMsg
	return (*c).JSON(http.StatusOK, res)
}

func Int64ToBytes(i int64) []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(i))
	return buf
}

func doAuth(c echo.Context) bool {
	token := c.Request().Header.Get("token") //nolint:canonicalheader // private header
	if token == "" {
		token = c.QueryParam("token")
	}
	return myDice.Parent.AccessTokens.Exists(token)
}

func GetHexData(c echo.Context, method string, name string) (value []byte, finished bool) {
	var err error
	var strValue string
	// var exists bool

	switch method {
	case "GET":
		strValue = c.Param(name)
	case "POST":
		strValue = c.FormValue(name)
	}

	// if !exists {
	// 	c.String(http.StatusNotAcceptable, "")
	// 	return nil, true
	// }

	value, err = hex.DecodeString(strValue)
	if err != nil {
		_ = c.String(http.StatusBadRequest, "")
		return nil, true
	}

	return value, false
}

var getAvatarCounter = 0

func getGithubAvatar(c echo.Context) error {
	getAvatarCounter++
	if getAvatarCounter > 500 {
		// 请求次数过多
		return c.JSON(http.StatusNotFound, "")
	}

	uid := c.Param("uid")
	req := request.Client{
		URL:    fmt.Sprintf("https://avatars.githubusercontent.com/%s?s=200", uid),
		Method: "GET",
	}

	resp := req.Send()
	if resp.OK() {
		// 设置缓存时间为3天
		c.Response().Header().Set("Cache-Control", "max-age=259200")

		return c.Blob(http.StatusOK, resp.ContentType(), resp.Bytes())
	}
	return c.JSON(http.StatusNotFound, "")
}

func packGocqConfig(relWorkDir string) *bytes.Buffer {
	// workDir := "extra/go-cqhttp-qq" + account
	rootPath := filepath.Join(myDice.BaseConfig.DataDir, relWorkDir)

	// 创建一个内存缓冲区，用于保存 Zip 文件内容
	buf := new(bytes.Buffer)

	// 创建 Zip Writer，将 Zip 文件内容写入内存缓冲区
	zipWriter := zip.NewWriter(buf)

	if err := compressFile(filepath.Join(rootPath, "config.yml"), "config.yml", zipWriter); err != nil {
		myDice.Logger.Error(err)
	}
	if err := compressFile(filepath.Join(rootPath, "device.json"), "device.json", zipWriter); err != nil {
		myDice.Logger.Error(err)
	}
	_ = compressFile(filepath.Join(rootPath, "data/versions/1.json"), "data/versions/6.json", zipWriter)
	_ = compressFile(filepath.Join(rootPath, "data/versions/6.json"), "data/versions/6.json", zipWriter)

	// 关闭 Zip Writer
	if err := zipWriter.Close(); err != nil {
		myDice.Logger.Fatal(err)
	}

	// 将 Zip 文件保存在内存中
	return buf
}

func compressFile(fn string, zipFn string, zipWriter *zip.Writer) error {
	data, err := os.ReadFile(fn)
	if err != nil {
		return err
	}

	h := &zip.FileHeader{Name: zipFn, Method: zip.Deflate, Flags: 0x800}
	fileWriter, err := zipWriter.CreateHeader(h)
	if err != nil {
		return err
	}
	_, _ = fileWriter.Write(data)
	return nil
}

func checkUidExists(c echo.Context, uid string) bool {
	for _, i := range myDice.ImSession.EndPoints {
		if pa, ok := i.Adapter.(*dice.PlatformAdapterGocq); ok && pa.UseInPackClient {
			var relWorkDir string
			switch pa.BuiltinMode {
			case "lagrange":
				relWorkDir = "extra/lagrange-qq" + uid
			case "lagrange-gocq":
				relWorkDir = "extra/lagrange-gocq-qq" + uid
			default:
				// 默认为gocq
				relWorkDir = "extra/go-cqhttp-qq" + uid
			}
			if relWorkDir == i.RelWorkDir {
				// 不允许工作路径重复
				_ = c.JSON(CodeAlreadyExists, i)
				return true
			}
		}

		// 如果存在已经启用的同账号连接，不允许重复
		if i.Enable && i.UserID == dice.FormatDiceIDQQ(uid) {
			_ = c.JSON(CodeAlreadyExists, i)
			return true
		}
	}
	return false
}

const (
	checkTimes   = 3
	checkTimeout = 5 * time.Second
)

func checkHTTPConnectivity(url string) bool {
	client := http.Client{
		Timeout: checkTimeout,
	}
	rsChan := make(chan bool, checkTimes)
	once := func(wg *sync.WaitGroup, url string) {
		defer wg.Done()
		resp, err := client.Get(url)
		myDice.Logger.Debugf("check http connectivity, url=%s", url)
		if err == nil {
			_ = resp.Body.Close()
			rsChan <- true
		} else {
			myDice.Logger.Debugf("url can't be connected, error: %s", err)
			rsChan <- false
		}
	}

	var wg sync.WaitGroup
	wg.Add(checkTimes)
	for range checkTimes {
		go once(&wg, url)
	}
	go func() {
		wg.Wait()
		close(rsChan)
	}()

	ok := true
	for res := range rsChan {
		ok = ok && res
	}
	return ok
}

var (
    signURLsCache      []string
    signURLsCacheAt    time.Time
    signURLsCacheMutex sync.Mutex
    signURLsTTL        = 10 * time.Minute
)

func getSignProbeURLs() []string {
    signURLsCacheMutex.Lock()
    defer signURLsCacheMutex.Unlock()

    if time.Since(signURLsCacheAt) < signURLsTTL && len(signURLsCache) > 0 {
        return append([]string(nil), signURLsCache...)
    }
    urls := fetchSignProbeURLsFromConn()
    urls = normalizeAndUniqURLs(urls)
    if len(urls) > 0 {
        signURLsCache = urls
        signURLsCacheAt = time.Now()
        return append([]string(nil), urls...)
    }
    return nil
}

func fetchSignProbeURLsFromConn() []string {
    info, err := dice.LagrangeGetSignInfo(myDice)
    if err != nil || info == nil {
        myDice.Logger.Debugf("LagrangeGetSignInfo failed: %v", err)
        return nil
    }

    var urls []string
    switch v := any(info).(type) {
    case map[string]any:
        urls = extractURLsFromMap(v)
        if len(urls) == 0 {
            hosts := extractHostsFromMap(v)
            for _, h := range hosts {
                base := ensureScheme(strings.TrimRight(h, "/"))
                urls = append(urls, base+"/api/sign/30366")
            }
        }
    default:
        b, _ := json.Marshal(info)
        var m map[string]any
        _ = json.Unmarshal(b, &m)
        urls = extractURLsFromMap(m)
        if len(urls) == 0 {
            hosts := extractHostsFromMap(m)
            for _, h := range hosts {
                base := ensureScheme(strings.TrimRight(h, "/"))
                urls = append(urls, base+"/api/sign/30366")
            }
        }
    }

    return normalizeAndUniqURLs(urls)
}

func ensureScheme(s string) string {
    if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
        return s
    }
    return "https://" + s
}

func extractURLsFromMap(m map[string]any) []string {
    var urls []string
    candidates := []string{"SignURLs", "sign_urls", "URLs", "urls", "Endpoints", "endpoints"}
    for _, key := range candidates {
        if v, ok := m[key]; ok {
            if arr, ok := v.([]any); ok {
                for _, x := range arr {
                    if s, ok := x.(string); ok && s != "" {
                        urls = append(urls, s)
                    }
                }
            }
        }
    }
    return urls
}

func extractHostsFromMap(m map[string]any) []string {
    var hosts []string
    for _, key := range []string{"Hosts", "hosts", "Servers", "servers"} {
        if v, ok := m[key]; ok {
            if arr, ok := v.([]any); ok {
                for _, x := range arr {
                    if s, ok := x.(string); ok && s != "" {
                        hosts = append(hosts, s)
                    }
                }
            }
        }
    }
    for _, key := range []string{"BaseURL", "base_url"} {
        if v, ok := m[key]; ok {
            if s, ok := v.(string); ok && s != "" {
                hosts = append(hosts, s)
            }
        }
    }
    return hosts
}

func normalizeAndUniqURLs(in []string) []string {
    seen := make(map[string]struct{}, len(in))
    var out []string
    for _, u := range in {
        u = strings.TrimSpace(u)
        if u == "" {
            continue
        }
        u = ensureScheme(u)
        for strings.Contains(u, "///") {
            u = strings.ReplaceAll(u, "///", "/")
        }
        if _, ok := seen[u]; !ok {
            seen[u] = struct{}{}
            out = append(out, u)
        }
    }
    return out
}

func checkStrictSignURL(url string, attempts int) bool {
    if attempts <= 0 {
        attempts = 1
    }
    client := http.Client{
        Timeout: 15 * time.Second,
    }

    for i := 0; i < attempts; i++ {
        resp, err := client.Get(url)
        myDice.Logger.Debugf("sign probe: %s (try %d/%d)", url, i+1, attempts)
        if err != nil {
            myDice.Logger.Debugf("sign probe error: %v", err)
            return false
        }

        n, _ := io.CopyN(io.Discard, resp.Body, 1)
        _ = resp.Body.Close()

        if resp.StatusCode != http.StatusOK {
            myDice.Logger.Debugf("sign probe bad status: %d", resp.StatusCode)
            return false
        }

        if n == 0 {
            myDice.Logger.Debugf("sign probe empty body")
            return false
        }

        if strings.HasPrefix(url, "https://") {
            if resp.TLS == nil || len(resp.TLS.VerifiedChains) == 0 {
                myDice.Logger.Debugf("sign probe tls not verified")
                return false
            }
        }
    }
    return true
}

func checkNetworkHealth(c echo.Context) error {
    total := 5 // baidu, seal, sign, google, github
    var ok []string
    var wg sync.WaitGroup
    wg.Add(total)
    rsChan := make(chan string, total)

    checkUrls := func(target string, urls []string) {
        defer wg.Done()
        for _, url := range urls {
            if checkHTTPConnectivity(url) {
                rsChan <- target
                break
            }
        }
    }
    go checkUrls("baidu", []string{"https://baidu.com"})
    go checkUrls("seal", dice.BackendUrls)
	go func() {
	    defer wg.Done()
	    urls := getSignProbeURLs()
	    if len(urls) == 0 {
	        // 没有下发任何签名地址，直接视为失败（不写 rsChan）
	        myDice.Logger.Debugf("no sign endpoints were delivered")
	        return
	    }
	    // 逐个地址探测：只要某个地址“连续 checkTimes 次严格成功”，就算 sign 可用
	    for _, u := range urls {
	        if checkStrictSignURL(u, checkTimes) { // ✅ 沿用老逻辑的次数（checkTimes）
	            rsChan <- "sign"
	            return
	        }
	    }
	    // 全部地址都没达到“连续 checkTimes 次成功”，不写入 rsChan（前端就看不到 sign）
	}()
    go checkUrls("google", []string{"https://google.com"})
    go checkUrls("github", []string{"https://github.com"})

    go func() {
        wg.Wait()
        close(rsChan)
    }()

    for targetOk := range rsChan {
        ok = append(ok, targetOk)
    }

    return Success(&c, Response{
        "total":     total,
        "ok":        ok,
        "timestamp": time.Now().Unix(),
    })
}
