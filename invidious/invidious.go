package invidious

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/sirupsen/logrus"
)

const timeoutDuration = 10 * time.Minute
const maxSizeMB = 50
const instancesEndpoint = "https://api.invidious.io/instances.json?sort_by=api,type"
const videosEndpoint = "https://%s/api/v1/videos/%s?fields=videoId,title,description,author,lengthSeconds,size,formatStreams"

var expireRegex = regexp.MustCompile(`(?i)expire=(\d+)`)
var logger = logrus.New()

type Timeout struct {
	Instance  string
	Timestamp time.Time
}

type Client struct {
	http     *http.Client
	timeouts []Timeout
	Instance string
}

type Format struct {
	VideoId   string
	Name      string `json:"qualityLabel"`
	Height    int
	Width     int
	Url       string `json:"url"`
	Container string `json:"container"`
	Size      string `json:"size"`
}

type Video struct {
	VideoId     string   `json:"videoId"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Uploader    string   `json:"author"`
	Duration    int      `json:"lengthSeconds"`
	Formats     []Format `json:"formatStreams"`
	Timestamp   time.Time
	Expire      time.Time
	FormatIndex int
}

func filter[T any](ss []T, test func(T) bool) (ret []T) {
	for _, s := range ss {
		if test(s) {
			ret = append(ret, s)
		}
	}
	return
}

func parseOrZero(number string) int {
	res, err := strconv.Atoi(number)
	if err != nil {
		return 0
	}
	return res
}

type HTTPError struct {
	StatusCode int
}

func (e HTTPError) Error() string {
	return fmt.Sprintf("HTTP error: %d", e.StatusCode)
}

func (c *Client) fetchVideo(videoId string) (*Video, error) {
	if c.Instance == "" {
		err := c.NewInstance()
		if err != nil {
			logger.Fatal(err, "Could not get a new instance.")
		}
	}
	endpoint := fmt.Sprintf(videosEndpoint, c.Instance, url.QueryEscape(videoId))
	resp, err := c.http.Get(endpoint)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, HTTPError{resp.StatusCode}
	}

	res := &Video{}
	err = json.Unmarshal(body, res)
	if err != nil {
		return nil, err
	}

	mp4Test := func(f Format) bool { return f.Container == "mp4" }
	res.Formats = filter(res.Formats, mp4Test)

	expireString := expireRegex.FindStringSubmatch(res.Formats[0].Url)
	expireTimestamp, err := strconv.ParseInt(expireString[1], 10, 64)
	if err != nil {
		fmt.Println("Error:", err)
		return nil, err
	}
	res.Expire = time.Unix(expireTimestamp, 0)

	return res, err
}

func (c *Client) GetVideo(videoId string) (*Video, error) {
	logger.Info("Video https://youtu.be/", videoId, " was requested.")

	video, err := GetVideoDB(videoId)
	if err == nil {
		logger.Info("Found a valid cache entry.")
		return video, nil
	}

	video, err = c.fetchVideo(videoId)

	if err != nil {
		if httpErr, ok := err.(HTTPError); ok {
			// handle HTTPError
			if httpErr.StatusCode == http.StatusNotFound {
				logger.Debug("Video does not exist.")
				return nil, err
			}
			logger.Debug("Invidious HTTP error: ", httpErr.StatusCode)
		}
		// handle generic error
		logger.Error(err)
		err = c.NewInstance()
		if err != nil {
			logger.Error("Could not get a new instance: ", err)
			time.Sleep(10 * time.Second)
		}
		return c.GetVideo(videoId)
	}
	logger.Info("Retrieved by API.")

	CacheVideoDB(*video)
	return video, nil
}

func (c *Client) isNotTimedOut(instance string) bool {
	for i := range c.timeouts {
		cur := c.timeouts[i]
		if instance == cur.Instance {
			return false
		}
	}
	return true
}

func (c *Client) NewInstance() error {
	now := time.Now()

	timeoutsTest := func(t Timeout) bool { return now.Sub(t.Timestamp) < timeoutDuration }
	c.timeouts = filter(c.timeouts, timeoutsTest)

	timeout := Timeout{c.Instance, now}
	c.timeouts = append(c.timeouts, timeout)

	resp, err := c.http.Get(instancesEndpoint)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf(string(body))
	}

	var jsonArray [][]interface{}
	err = json.Unmarshal(body, &jsonArray)
	if err != nil {
		return err
	}

	for i := range jsonArray {
		instance := jsonArray[i][0].(string)
		instanceTest := func(t Timeout) bool { return t.Instance == instance }
		result := filter(c.timeouts, instanceTest)
		if len(result) == 0 {
			c.Instance = instance
			logger.Info("Using new instance: ", c.Instance)
			return nil
		}
	}
	logger.Error("Cannot find a valid instance.")
	return err
}

func (c *Client) ProxyVideo(w http.ResponseWriter, videoId string, formatIndex int) error {
	video, err := GetVideoDB(videoId)
	if err != nil {
		logger.Debug("Cannot proxy a video that is not cached: https://youtu.be/", videoId)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return err
	}

	fmtAmount := len(video.Formats)
	idx := formatIndex % fmtAmount
	url := video.Formats[fmtAmount-1-idx].Url
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		logger.Error(err)
		new_video, err := c.fetchVideo(videoId)
		if err != nil {
			logger.Error("Url for", videoId, "expired:", err)
			return err
		}
		return c.ProxyVideo(w, new_video.VideoId, formatIndex)
	}

	req.Header.Add("Range", fmt.Sprintf("bytes=0-%d000000", maxSizeMB))
	resp, err := c.http.Do(req)
	if err != nil {
		logger.Error(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return err
	}
	defer resp.Body.Close()

	w.Header().Set("content-type", "video/mp4")
	w.Header().Set("Status", "200")

	temp := bytes.NewBuffer(nil)
	_, err = io.Copy(temp, resp.Body)
	if err == nil { // done
		_, err = io.Copy(w, temp)
		return err
	}

	newIndex := formatIndex + 1
	if newIndex < fmtAmount {
		return c.ProxyVideo(w, videoId, newIndex)
	}
	_, err = io.Copy(w, temp)
	return err
}

func NewClient(httpClient *http.Client) *Client {
	InitDB()
	return &Client{
		http:     httpClient,
		timeouts: []Timeout{},
		Instance: "",
	}
}
