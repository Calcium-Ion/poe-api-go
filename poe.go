package poe_api_go

import (
	"bytes"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dop251/goja"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/gorilla/websocket"
	"github.com/zhangyunhao116/skipmap"
)

type Client struct {
	token          string
	deviceID       string
	proxy          *url.URL
	session        tls_client.HttpClient
	activeMessages *skipmap.StringMap[float64]
	messageQueues  *skipmap.StringMap[chan map[string]interface{}]
	headers        fhttp.Header
	formKey        string
	viewer         map[string]interface{}
	userID         string
	nextData       map[string]interface{}
	channel        map[string]interface{}
	bots           map[string]interface{}
	botNames       map[string]string
	gqlHeaders     http.Header
	wsDomain       string
	wsConn         *websocket.Conn
	wsConnected    bool
	requestCount   atomic.Int64
}

func NewClient(token string, formKey string, proxy *url.URL) (*Client, error) {
	// Initialize the client
	client := &Client{
		token:          token,
		deviceID:       "",
		proxy:          proxy,
		headers:        headers,
		activeMessages: skipmap.NewString[float64](),
		messageQueues:  skipmap.NewString[chan map[string]interface{}](),
	}
	// Set up the session
	err := client.setupSession(token)
	if err != nil {
		return nil, err
	}
	// Set up the connection
	err = client.setupConnection(formKey)
	if err != nil {
		return nil, err
	}
	client.connectWs()

	return client, nil
}

func (c *Client) GetBots() map[string]string {
	return c.botNames
}

func (c *Client) SendMessage(chatbot, message string, stream bool, timeout time.Duration) (<-chan map[string]interface{}, error) {
	// 支持通过name获取chatbot 而不需要拿到poe后端需要的name
	//log.Printf("Bots: %v", c)

	if name, ok := c.botNames[chatbot]; ok {
		chatbot = name
	}
	result := make(chan map[string]interface{})
	timer := 0 * time.Second
	// 防止并发 这里要先检查下是否有仍然未完成的消息
	for c.activeMessages.Len() != 0 {
		time.Sleep(10 * time.Millisecond)
		timer += 10 * time.Millisecond
		if timer > timeout {
			return nil, errors.New("timed out waiting for other messages to send")
		}
	}
	log.Printf("Sending message to %s: %s", chatbot, message)

	if !c.wsConnected {
		c.disconnectWs()
		err := c.setupConnection(c.formKey)
		if err != nil {
			return nil, err
		}
		c.connectWs()
	}

	chatID := c.getBotByCodename(chatbot)["chatId"].(float64)
	//messageData :=
	messageData, _ := c.sendQuery("SendMessageMutation", map[string]interface{}{
		"bot":           chatbot,
		"query":         message,
		"chatId":        chatID,
		"source":        nil,
		"clientNonce":   generateNonce(16),
		"sdid":          c.deviceID,
		"withChatBreak": stream,
	}, 0)

	if messageData["data"].(map[string]interface{})["messageEdgeCreate"].(map[string]interface{})["message"] == nil {
		return nil, fmt.Errorf("daily limit reached for %s", chatbot)
	}

	humanMessage := messageData["data"].(map[string]interface{})["messageEdgeCreate"].(map[string]interface{})["message"].(map[string]interface{})
	humanMessageIDFloat64 := humanMessage["node"].(map[string]interface{})["messageId"].(float64)
	humanMessageID := fmt.Sprintf("%v", humanMessageIDFloat64)
	c.activeMessages.Store(humanMessageID, 0)
	c.messageQueues.Store(humanMessageID, make(chan map[string]interface{}, 1))
	var lastChan = make(chan string, 1)
	go c.dealMessage(humanMessageID, lastChan, result, timeout)
	go c.sendRecv(humanMessageID, chatbot, chatbot, lastChan)
	return result, nil
}

func (c *Client) SendChatBreak(chatbot string) (map[string]interface{}, error) {
	log.Printf("Sending chat break to %s", chatbot)
	result, _ := c.sendQuery("AddMessageBreakMutation", map[string]interface{}{
		"chatId": c.getBotByCodename(chatbot)["chatId"],
	}, 0)
	return result["data"].(map[string]interface{})["messageBreakCreate"].(map[string]interface{})["message"].(map[string]interface{}), nil
}

func (c *Client) GetMessageHistory(chatbot string, count int, cursor interface{}) ([]map[string]interface{}, error) {
	log.Printf("Downloading %d messages from %s", count, chatbot)

	messages := []map[string]interface{}{}

	if cursor == nil {
		chatData := c.getBot(chatbot)
		if len(chatData["messagesConnection"].(map[string]interface{})["edges"].([]interface{})) == 0 {
			return []map[string]interface{}{}, nil
		}

		edges := chatData["messagesConnection"].(map[string]interface{})["edges"].([]map[string]interface{})
		messages = edges[int(math.Max(float64(len(edges)-count), 0)):]
		cursor = chatData["messagesConnection"].(map[string]interface{})["pageInfo"].(map[string]interface{})["startCursor"]
		count -= len(messages)
	}

	if count <= 0 {
		return messages, nil
	}

	if count > 50 {
		var err error
		messages, err = c.GetMessageHistory(chatbot, 50, cursor)
		if err != nil {
			return nil, err
		}
		for count > 0 {
			count -= 50
			newCursor := messages[0]["cursor"].(string)
			newMessages, err := c.GetMessageHistory(chatbot, min(50, count), newCursor)
			if err != nil {
				return nil, err
			}
			messages = append(newMessages, messages...)
		}
		return messages, nil
	}

	result, _ := c.sendQuery("ChatListPaginationQuery", map[string]interface{}{
		"count":  count,
		"cursor": cursor,
		"id":     c.getBotByCodename(chatbot)["id"].(string),
	}, 0)
	queryMessages := result["data"].(map[string]interface{})["node"].(map[string]interface{})["messagesConnection"].(map[string]interface{})["edges"].([]map[string]interface{})
	messages = append(queryMessages, messages...)
	return messages, nil
}

func (c *Client) DeleteMessage(messageIDs []int) error {
	log.Printf("Deleting messages: %v", messageIDs)
	_, err := c.sendQuery("DeleteMessageMutation", map[string]interface{}{
		"messageIds": messageIDs,
	}, 0)
	if err != nil {
		return err
	}
	return nil
}

func (c *Client) PurgeConversation(chatbot string, count int) error {
	log.Printf("Purging messages from %s", chatbot)
	lastMessages, err := c.GetMessageHistory(chatbot, 50, nil)
	if err != nil {
		return err
	}
	reverseSlice(lastMessages)

	for len(lastMessages) > 0 {
		var messageIDs []int

		for _, message := range lastMessages {
			if count == 0 {
				break
			}
			count--
			messageID := int(message["node"].(map[string]interface{})["messageId"].(float64))
			messageIDs = append(messageIDs, messageID)
		}

		err := c.DeleteMessage(messageIDs)
		if err != nil {
			return err
		}

		if count == 0 {
			return nil
		}
		lastMessages, err = c.GetMessageHistory(chatbot, 50, nil)
		if err != nil {
			return err
		}
		reverseSlice(lastMessages)
	}

	log.Printf("No more messages left to delete.")
	return nil
}

func (c *Client) CreateBot(req CreateBot) (map[string]interface{}, error) {
	if req.PromptPublic == nil {
		req.PromptPublic = GetPoint[bool](true)
	}
	if req.MarkdownRendering == nil {
		req.MarkdownRendering = GetPoint[bool](true)
	}
	result, _ := c.sendQuery("PoeBotCreateMutation", map[string]interface{}{
		"baseBot":              req.BaseModel,
		"displayName":          req.DisplayName,
		"handle":               req.Handle,
		"prompt":               req.Prompt,
		"isPromptPublic":       req.PromptPublic,
		"introduction":         req.IntroMessage,
		"description":          req.Description,
		"profilePictureUrl":    req.PfpUrl,
		"apiUrl":               req.ApiUrl,
		"apiKey":               req.ApiKey,
		"isApiBot":             req.ApiBot,
		"hasLinkification":     req.Linkification,
		"hasMarkdownRendering": req.MarkdownRendering,
		"hasSuggestedReplies":  req.SuggestedReplies,
		"isPrivateBot":         req.Private,
		"temperature":          req.Temperature,
	}, 0)
	data := getMap(getMap(result, "data"), "poeBotCreate")
	if data["status"] != "success" {
		return nil, errors.New("Poe returned an error while trying to create a bot ")
	}
	_, err := c.getBots(false)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (c *Client) EditBot(botID string, req CreateBot) (map[string]interface{}, error) {
	if req.PromptPublic == nil {
		req.PromptPublic = GetPoint[bool](true)
	}
	if req.MarkdownRendering == nil {
		req.MarkdownRendering = GetPoint[bool](true)
	}
	result, _ := c.sendQuery("PoeBotEditMutation", map[string]interface{}{
		"botId":                botID,
		"baseBot":              req.BaseModel,
		"displayName":          req.DisplayName,
		"handle":               req.Handle,
		"prompt":               req.Prompt,
		"isPromptPublic":       req.PromptPublic,
		"introduction":         req.IntroMessage,
		"description":          req.Description,
		"profilePictureUrl":    req.PfpUrl,
		"apiUrl":               req.ApiUrl,
		"apiKey":               req.ApiKey,
		"isApiBot":             req.ApiBot,
		"hasLinkification":     req.Linkification,
		"hasMarkdownRendering": req.MarkdownRendering,
		"hasSuggestedReplies":  req.SuggestedReplies,
		"isPrivateBot":         req.Private,
		"temperature":          req.Temperature,
	}, 0)
	data := getMap(getMap(result, "data"), "poeBotEdit")
	if data["status"] != "success" {
		return nil, errors.New("Poe returned an error while trying to create a bot ")
	}
	_, err := c.getBots(false)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (c *Client) requestWithRetries(method string, url string, attempts int, data []byte, headers map[string][]string) (*fhttp.Response, error) {
	if attempts == 0 {
		attempts = 3
	}
	client := c.session
	var payload io.Reader
	if data != nil {
		payload = bytes.NewBuffer(data)
	}
	req, err := fhttp.NewRequest(method, url, payload)
	if err != nil {
		return nil, err
	}
	req.Header = c.headers.Clone()
	if headers != nil {
		for key, value := range headers {
			req.Header[key] = value
		}
	}

	for i := 0; i < attempts; i++ {
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		if resp.StatusCode == http.StatusTemporaryRedirect {
			body, _ := io.ReadAll(resp.Body)
			if strings.HasPrefix(resp.Header.Get("Location"), "/login") {
				return nil, fmt.Errorf("invalid token: %s", c.token)
			}
			fmt.Println(body)
		}
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == 400 {
			// print the response body
			body, _ := io.ReadAll(resp.Body)
			logger.Println(string(body))
			logger.Println(req.Header)
			logger.Println(string(data))
		}
		logger.Printf("Server returned a status code of %d while downloading %s. Retrying (%d/%d)...", resp.StatusCode, url, i+1, attempts)
		time.Sleep(time.Second)
	}

	return nil, fmt.Errorf("failed to download %s too many times", url)
}

func (c *Client) setupSession(token string) error {
	// Set up the session with the provided token and proxy
	jar := tls_client.NewCookieJar()
	options := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(30),
		tls_client.WithClientProfile(tls_client.Okhttp4Android13),
		// tls_client.WithNotFollowRedirects(),
		tls_client.WithCookieJar(jar), // create cookieJar instance and pass it as argument
	}

	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)
	if err != nil {
		log.Println(err)
		return nil
	}
	c.session = client

	if c.proxy != nil {
		err := c.session.SetProxy(c.proxy.String())
		if err != nil {
			return err
		}
		log.Printf("Proxy enabled: %s\n", c.proxy.String())
	}

	// Update session headers
	// log.Println(c.headers)
	for key, value := range headers {
		c.headers[key] = value
	}
	// Set cookie
	cookie := &fhttp.Cookie{
		Name:   "p-b",
		Value:  token,
		Domain: "poe.com",
	}

	uri, err := url.Parse(homeURL)
	if err != nil {
		return err
	}
	c.session.SetCookies(uri, []*fhttp.Cookie{cookie})
	return nil
}

func (c *Client) setupConnection(formKey string) error {
	c.wsDomain = fmt.Sprintf("tch%d", rand.Intn(1000000))
	nextData, err := c.getNextData(true, formKey)
	if err != nil {
		return err
	}
	c.nextData = nextData
	c.channel, _ = c.getChannelData()
	c.bots, _ = c.getBots(false)
	c.botNames = c.getBotNames()

	if c.deviceID == "" {
		c.deviceID = c.getDeviceID()
	}

	c.gqlHeaders = make(http.Header)
	c.gqlHeaders.Set("poe-formkey", c.formKey)
	// c.gqlHeaders.Set("Poe-Tag-Id", "ccb25cfc4ed7004c8ed68f5fa0a8760b")
	c.gqlHeaders.Set("poe-tchannel", c.channel["channel"].(string))

	for k, v := range c.headers {
		c.gqlHeaders[k] = v
	}

	c.subscribe()
	return nil
}

func (c *Client) getDeviceID() string {
	userID := c.viewer["poeUser"].(map[string]interface{})["id"].(string)
	deviceID := getSavedDeviceID(userID)
	log.Printf("Device ID: %s", deviceID)
	return deviceID
}

func (c *Client) extractFormKey(html string) string {
	scriptRegex := regexp.MustCompile(`<script>(.+?)</script>`)
	scriptText := `
      let QuickJS = undefined, process = undefined;
      let window = new Proxy({
        document: {a:1},
        navigator: {a:1}
      },{
        get(obj, prop) {
          return obj[prop] || true;
        },
        set(obj, prop, value) {
          obj[prop] = value;
          return true;
        }
      });
    `
	matches := scriptRegex.FindAllStringSubmatch(html, -1)
	for _, match := range matches {
		scriptText += match[1]
	}

	functionRegex := regexp.MustCompile(`(window\.[a-zA-Z0-9]{17})=function`)
	functionText := functionRegex.FindStringSubmatch(scriptText)[1]
	scriptText += functionText + "();"

	rt := goja.New()
	formkey, err := rt.RunString(scriptText)
	if err != nil {
		log.Printf("Error evaluating script: %s", err)
		return ""
	}
	//log.Printf("Formkey: %s", formkey.String())
	return formkey.String()
}

func (c *Client) getNextData(overwriteVars bool, formKey string) (map[string]interface{}, error) {
	resp, err := c.requestWithRetries(http.MethodGet, homeURL, 0, nil, nil)
	if err != nil {
		return nil, err
	}

	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			log.Printf("Error closing response body: %s", err)
		}
	}(resp.Body)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	jsonRegex := regexp.MustCompile(`<script id="__NEXT_DATA__" type="application\/json">(.+?)</script>`)
	jsonText := jsonRegex.FindStringSubmatch(string(body))[1]

	var nextData map[string]interface{}
	err = json.Unmarshal([]byte(jsonText), &nextData)
	if err != nil {
		return nil, err
	}
	if overwriteVars {
		if formKey == "" {
			c.formKey = c.extractFormKey(string(body))
		} else {
			c.formKey = formKey
		}
		log.Printf("Formkey: %s", c.formKey)
		//log.Printf("NextData: %s", jsonText)

		if containKey("payload", nextData["props"].(map[string]interface{})["pageProps"].(map[string]interface{})) {
			c.viewer = nextData["props"].(map[string]interface{})["pageProps"].(map[string]interface{})["payload"].(map[string]interface{})["viewer"].(map[string]interface{})
		} else {
			c.viewer = nextData["props"].(map[string]interface{})["pageProps"].(map[string]interface{})["data"].(map[string]interface{})["viewer"].(map[string]interface{})
		}
		if c.viewer["poeUser"] == nil {
			return nil, errors.New("invalid token or no bots are available")
		}
		c.userID = c.viewer["poeUser"].(map[string]interface{})["id"].(string)
		c.nextData = nextData
	}

	return nextData, nil
}

func (c *Client) getBot(displayName string) map[string]interface{} {
	url := fmt.Sprintf("https://poe.com/_next/data/%s/%s.json", c.nextData["buildId"].(string), displayName)

	resp, err := c.requestWithRetries(http.MethodGet, url, 0, nil, nil)
	if err != nil {
		// handle error
	}

	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)

	var jsonData map[string]interface{}
	err = json.Unmarshal(body, &jsonData)

	var chatData map[string]interface{}
	if containKey("payload", jsonData["pageProps"].(map[string]interface{})) {
		chatData = jsonData["pageProps"].(map[string]interface{})["payload"].(map[string]interface{})["chatOfBotHandle"].(map[string]interface{})
	} else {
		chatData = jsonData["pageProps"].(map[string]interface{})["data"].(map[string]interface{})["chatOfBotHandle"].(map[string]interface{})
	}
	return chatData
}

func (c *Client) getBots(downloadNextData bool) (map[string]interface{}, error) {
	if _, ok := c.viewer["availableBotsConnection"]; !ok {
		return nil, errors.New("invalid token or no bots are available")
	}
	botList := c.viewer["availableBotsConnection"].(map[string]interface{})["edges"].([]interface{})

	var wg sync.WaitGroup
	bots := make(map[string]interface{})
	lock := &sync.Mutex{}

	getBotThread := func(bot map[string]interface{}) {
		defer wg.Done()
		lock.Lock()
		defer lock.Unlock()
		chatData := c.getBot(bot["node"].(map[string]interface{})["handle"].(string))
		bots[chatData["defaultBotObject"].(map[string]interface{})["nickname"].(string)] = chatData
	}

	wg.Add(len(botList))
	for _, bot := range botList {
		go getBotThread(bot.(map[string]interface{}))
	}
	wg.Wait()

	c.bots = bots
	c.botNames = c.getBotNames()
	return bots, nil
}

func (c *Client) getBotByCodename(botCodename string) map[string]interface{} {
	if bot, ok := c.bots[botCodename]; ok {
		return bot.(map[string]interface{})
	}
	// TODO: Cache this so it isn't re-downloaded every time
	return c.getBot(botCodename)
}

func (c *Client) getBotNames() map[string]string {
	result := map[string]string{}
	for k, v := range c.bots {
		object := v.(map[string]interface{})["defaultBotObject"].(map[string]interface{})
		if name, ok := object["displayName"]; ok {
			result[name.(string)] = k
		}
	}
	return result
}

func (c *Client) exploreBots(endCursor *string, count int) (map[string]interface{}, error) {
	var url string
	var resp *fhttp.Response
	var err error

	if endCursor == nil {
		url = fmt.Sprintf("https://poe.com/_next/data/%s/explore_bots.json", c.nextData["buildId"].(string))
		resp, err = c.requestWithRetries(http.MethodGet, url, 0, nil, nil)
	} else {
		// Use GraphQL to get the next page
		queryData := map[string]interface{}{
			"count":  count,
			"cursor": *endCursor,
		}
		result, _ := c.sendQuery("ExploreBotsListPaginationQuery", queryData, 0)
		resultData := result["data"].(map[string]interface{})["exploreBotsConnection"].(map[string]interface{})

		bots := make([]map[string]interface{}, len(resultData["edges"].([]interface{})))
		for i, node := range resultData["edges"].([]interface{}) {
			bots[i] = node.(map[string]interface{})["node"].(map[string]interface{})
		}

		return map[string]interface{}{
			"bots":       bots,
			"end_cursor": resultData["pageInfo"].(map[string]interface{})["endCursor"],
		}, nil
	}

	// Handle error in HTTP response
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)

	if err != nil {
		return nil, err
	}

	var jsonData map[string]interface{}
	err = json.Unmarshal(body, &jsonData)

	if err != nil {
		return nil, err
	}

	nodes := jsonData["pageProps"].(map[string]interface{})["payload"].(map[string]interface{})["exploreBotsConnection"].(map[string]interface{})["edges"].([]interface{})
	bots := make([]map[string]interface{}, len(nodes))
	for i, node := range nodes {
		bots[i] = node.(map[string]interface{})["node"].(map[string]interface{})
	}

	return map[string]interface{}{
		"bots":       bots[:count],
		"end_cursor": jsonData["pageProps"].(map[string]interface{})["payload"].(map[string]interface{})["exploreBotsConnection"].(map[string]interface{})["pageInfo"].(map[string]interface{})["endCursor"],
	}, nil
}

func (c *Client) getRemainingMessages(chatbot string) int {
	chatData := c.getBotByCodename(chatbot)
	return int(chatData["defaultBotObject"].(map[string]interface{})["messageLimit"].(map[string]interface{})["numMessagesRemaining"].(float64))
}

func (c *Client) getChannelData() (map[string]interface{}, error) {
	log.Println("Downloading channel data...")
	resp, err := c.requestWithRetries(http.MethodGet, settingsURL, 0, nil, nil)

	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var jsonData map[string]interface{}
	err = json.Unmarshal(body, &jsonData)
	if err != nil {
		return nil, err
	}
	return jsonData["tchannelData"].(map[string]interface{}), nil
}

func (c *Client) getWebsocketURL(channel map[string]interface{}) string {
	if channel == nil {
		channel = c.channel
	}
	minSeq := channel["minSeq"].(string)
	channelName := channel["channel"].(string)
	hash := channel["channelHash"].(string)
	baseHost := channel["baseHost"].(string)
	boxName := channel["boxName"].(string)

	return fmt.Sprintf("wss://%s.tch.%s/up/%s/updates?min_seq=%s&channel=%s&hash=%s", c.wsDomain, baseHost, boxName, minSeq, channelName, hash)
}

func (c *Client) sendQuery(queryName string, variables interface{}, attempts int) (map[string]interface{}, error) {
	if attempts == 0 {
		attempts = 10
	}
	for i := 0; i < attempts; i++ {
		jsonData := generatePayload(queryName, variables)
		payload, _ := json.Marshal(jsonData)

		baseString := string(payload) + c.gqlHeaders["Poe-Formkey"][0] + "Jb1hi3fg1MxZpzYfy"

		headers := map[string][]string{
			"Content-Type": {"application/json"},
			"Poe-Tag-Id":   {fmt.Sprintf("%x", md5.Sum([]byte(baseString)))},
		}

		for k, v := range c.gqlHeaders {
			headers[k] = v
		}
		if queryName == "recv" {
			_, err := c.requestWithRetries(http.MethodPost, gqlRecvURL, attempts, payload, headers)
			if err != nil {
				//panic(err)
				log.Println(err)
			}
			return nil, nil
		}
		resp, err := c.requestWithRetries(http.MethodPost, gqlURL, attempts, payload, headers)

		// Handle error in HTTP response
		if err != nil {
			return nil, err
		}

		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)

		var data map[string]interface{}
		err = json.Unmarshal(body, &data)

		if data["data"] == nil {
			log.Println(string(payload))
			log.Printf("%s returned an error: %s | Retrying (%d/%d)\n", queryName, data["errors"].([]interface{})[0].(map[string]interface{})["message"].(string), i+1, attempts)
			time.Sleep(2 * time.Second)
			continue
		}

		return data, nil
	}

	//panic(fmt.Sprintf("%s failed too many times.", queryName))
	return nil, errors.New(fmt.Sprintf("%s failed too many times.", queryName))
}

func (c *Client) subscribe() map[string]interface{} {
	log.Println("Subscribing to mutations")
	var result, _ = c.sendQuery("SubscriptionsMutation", map[string]interface{}{
		"subscriptions": []map[string]interface{}{
			{
				"subscriptionName": "messageAdded",
				"query":            queries["MessageAddedSubscription"],
			},
			{
				"subscriptionName": "viewerStateUpdated",
				"query":            queries["ViewerStateUpdatedSubscription"],
			},
		},
	}, 0)
	return result
}

func (c *Client) wsRunThread() {
	dialer := websocket.DefaultDialer

	if c.proxy != nil {
		dialer.Proxy = http.ProxyURL(c.proxy)
	}

	conn, _, err := dialer.Dial(c.getWebsocketURL(nil), http.Header{"User-Agent": {userAgent}})
	if err != nil {
		log.Fatalf("Error connecting to websocket: %v", err)
	}
	c.wsConn = conn
	c.wsConnected = true

	for {
		_, message, err := c.wsConn.ReadMessage()
		if err != nil {
			c.onWsError(err)
			return
		}
		c.onMessage(message)
	}
}

func (c *Client) connectWs() {
	c.wsConnected = false
	go c.wsRunThread()

	for !c.wsConnected {
		time.Sleep(10 * time.Millisecond)
	}
}

func (c *Client) disconnectWs() {
	if c.wsConn != nil {
		c.wsConn.Close()
	}
	c.wsConnected = false
}

func (c *Client) onWsConnect() {
	c.wsConnected = true
}

func (c *Client) onWsClose(code int, text string) {
	c.wsConnected = false
	log.Printf("Websocket closed with status %d: %s", code, text)
}

func (c *Client) onWsError(err error) {
	c.disconnectWs()
	c.connectWs()
}

func (c *Client) onMessage(msg []byte) {
	var data map[string]interface{}
	err := json.Unmarshal(msg, &data)
	if err != nil {
		log.Printf("Error unmarshaling message: %v", err)
		c.disconnectWs()
		c.connectWs()
		return
	}

	messages, ok := data["messages"].([]interface{})
	if !ok {
		return
	}

	for _, messageStr := range messages {
		var messageData map[string]interface{}
		err := json.Unmarshal([]byte(messageStr.(string)), &messageData)
		if err != nil {
			log.Printf("Error unmarshaling message data: %v", err)
			c.disconnectWs()
			c.connectWs()
			return
		}

		if messageData["message_type"].(string) != "subscriptionUpdate" {
			continue
		}
		//t, err1 := json.Marshal(messageData)
		//if err1 != nil {
		//	log.Printf("wwwww")
		//}
		//log.Printf(string(t))
		// 先判断是否有messageAdded
		//var message map[string]interface{}
		if messageData["payload"].(map[string]interface{})["data"].(map[string]interface{})["messageAdded"] != nil {
			message := messageData["payload"].(map[string]interface{})["data"].(map[string]interface{})["messageAdded"].(map[string]interface{})
			copiedDict := make(map[string]float64)
			c.activeMessages.Range(func(key string, value float64) bool {
				copiedDict[key] = value
				return true
			})

			for key, value := range copiedDict {
				queue, ok := c.messageQueues.Load(key)
				if !ok {
					continue
				}
				if value == message["messageId"].(float64) {
					queue <- message
					return
				} else if key != "pending" && value == 0 && message["state"].(string) != "complete" {
					c.activeMessages.Store(key, message["messageId"].(float64))
					queue <- message
					return
				}
			}
		}
	}
}

func (c *Client) dealMessage(humanMessageID string, textCh chan string, result chan map[string]interface{}, timeout time.Duration) {
	defer c.activeMessages.Delete(humanMessageID)
	defer c.messageQueues.Delete(humanMessageID)
	defer close(result)
	defer close(textCh)
	lastText := ""
	messageID := ""
	ch, ok := c.messageQueues.Load(humanMessageID)
	if !ok {
		return
	}
	for {
		select {
		case <-time.After(timeout):
			return
		case message := <-ch:
			if message["state"].(string) == "complete" {
				if lastText != "" && fmt.Sprintf("%v", message["messageId"].(float64)) == messageID {
					return
				} else {
					continue
				}
			}

			textNew := message["text"].(string)[len(lastText):]
			lastText = message["text"].(string)
			messageID = fmt.Sprintf("%v", message["messageId"].(float64))
			textCh <- lastText
			message["text_new"] = textNew
			result <- message
		}
	}
}

func (c *Client) sendRecv(humanMessageID, chatbot, chatID string, textCh chan string) {
	//recovers from panic
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Recovered from panic: %v", r)
		}
	}()
	for text := range textCh {
		m := map[string]interface{}{
			"bot":                                 chatbot,
			"time_to_first_typing_indicator":      300,
			"time_to_first_subscription_response": 600,
			"time_to_full_bot_response":           1100,
			"full_response_length":                len(text) + 1,
			"full_response_word_count":            len(strings.Split(text, " ")) + 1,
			"human_message_id":                    humanMessageID,
			"chat_id":                             chatID,
			"bot_response_status":                 "success",
		}
		id, ok := c.activeMessages.Load(humanMessageID)
		if !ok || id == 0 {
			m["bot_message_id"] = nil
		} else {
			m["bot_message_id"] = id
		}
		_, _ = c.sendQuery("recv", m, 0)
	}
}
