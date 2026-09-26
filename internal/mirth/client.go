package mirth

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Client struct {
	mu         sync.Mutex
	baseURL    string
	username   string
	password   string
	tlsInsecure bool
	httpClient *http.Client
	loggedIn   bool
	lastError  string
}

func NewClient() *Client {
	return &Client{}
}

func (c *Client) Configure(baseURL, username, password string, tlsInsecure bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if tlsInsecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	c.baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	c.username = strings.TrimSpace(username)
	c.password = password
	c.tlsInsecure = tlsInsecure
	c.loggedIn = false
	c.lastError = ""
	c.httpClient = &http.Client{
		Timeout:   45 * time.Second,
		Jar:       jar,
		Transport: transport,
	}
	return nil
}

func (c *Client) Configured() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.baseURL != "" && c.username != "" && c.password != "" && c.httpClient != nil
}

func (c *Client) LastError() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastError
}

func (c *Client) setLastError(err error) {
	if err == nil {
		c.lastError = ""
		return
	}
	c.lastError = err.Error()
}

func (c *Client) Ping() error {
	if !c.Configured() {
		return fmt.Errorf("Mirth connection is not configured")
	}
	if err := c.ensureLogin(); err != nil {
		c.mu.Lock()
		c.setLastError(err)
		c.mu.Unlock()
		return err
	}
	_, _, err := c.request(http.MethodGet, "/api/server/status", nil, nil, true)
	c.mu.Lock()
	c.setLastError(err)
	c.mu.Unlock()
	return err
}

func (c *Client) ensureLogin() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loggedIn {
		return nil
	}
	return c.loginLocked()
}

func (c *Client) loginLocked() error {
	if c.httpClient == nil {
		return fmt.Errorf("Mirth client not configured")
	}
	form := url.Values{}
	form.Set("username", c.username)
	form.Set("password", c.password)
	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/api/users/_login", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "OpenAPI")
	req.Header.Set("Accept", "application/json, application/xml")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("login request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("login failed: %s (%s)", resp.Status, truncate(string(body), 200))
	}
	c.loggedIn = true
	return nil
}

func (c *Client) request(method, path string, query url.Values, body any, allowRelogin bool) ([]byte, string, error) {
	if !c.Configured() {
		return nil, "", fmt.Errorf("Mirth connection is not configured — open Settings and save credentials")
	}
	if err := c.ensureLogin(); err != nil {
		return nil, "", err
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, "", err
		}
		reader = bytes.NewReader(b)
	}
	c.mu.Lock()
	base := c.baseURL
	client := c.httpClient
	c.mu.Unlock()

	u := base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequest(method, u, reader)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("X-Requested-With", "OpenAPI")
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	ct := resp.Header.Get("Content-Type")
	if resp.StatusCode == http.StatusUnauthorized && allowRelogin {
		c.mu.Lock()
		c.loggedIn = false
		loginErr := c.loginLocked()
		c.mu.Unlock()
		if loginErr != nil {
			return nil, "", loginErr
		}
		return c.request(method, path, query, body, false)
	}
	if resp.StatusCode >= 300 {
		return nil, ct, fmt.Errorf("%s %s: %s (%s)", method, path, resp.Status, truncate(string(raw), 300))
	}
	return raw, ct, nil
}

func (c *Client) ServerStatus() (ConnectionMode, error) {
	if !c.Configured() {
		return ModeUnreachable, fmt.Errorf("not configured")
	}
	if err := c.Ping(); err != nil {
		return ModeUnreachable, err
	}
	return ModeConnected, nil
}

func (c *Client) GetDashboard() (DashboardSummary, error) {
	q := url.Values{}
	q.Set("includeUndeployed", "true")
	raw, _, err := c.request(http.MethodGet, "/api/channels/statuses", q, nil, true)
	if err != nil {
		return DashboardSummary{Mode: ModeUnreachable, ErrorMessage: err.Error(), RefreshedAt: time.Now()}, err
	}
	statuses, err := parseDashboardStatuses(raw)
	if err != nil {
		return DashboardSummary{Mode: ModeUnreachable, ErrorMessage: err.Error(), RefreshedAt: time.Now()}, err
	}
	summary := DashboardSummary{
		Mode:        ModeConnected,
		RefreshedAt: time.Now(),
		Channels:    make([]ChannelStats, 0),
	}
	for _, st := range statuses {
		statusType := strings.ToUpper(st.StatusType)
		if statusType != "" && statusType != "CHANNEL" {
			continue
		}
		stats := normalizeStats(st.Statistics)
		cs := ChannelStats{
			ChannelID: st.ChannelID,
			Name:      st.Name,
			State:     st.State,
			Received:  stats["RECEIVED"],
			Sent:      stats["SENT"],
			Error:     stats["ERROR"],
			Filtered:  stats["FILTERED"],
			Queued:    st.Queued,
		}
		if cs.Queued == 0 {
			cs.Queued = stats["QUEUED"]
		}
		summary.Channels = append(summary.Channels, cs)
		summary.ChannelCount++
		if strings.EqualFold(st.State, "Started") {
			summary.ChannelsStarted++
		}
		summary.Received += cs.Received
		summary.Sent += cs.Sent
		summary.Error += cs.Error
		summary.Filtered += cs.Filtered
		summary.Queued += cs.Queued
	}
	return summary, nil
}

func (c *Client) ListChannels() ([]ChannelInfo, error) {
	// Prefer lightweight idsAndNames
	if out, err := c.listChannelIDsAndNames(); err == nil && len(out) > 0 {
		return out, nil
	}
	raw, _, err := c.request(http.MethodGet, "/api/channels", nil, nil, true)
	if err != nil {
		return c.listChannelIDsAndNames()
	}
	out, err := parseChannelList(raw)
	if err != nil || len(out) == 0 {
		return c.listChannelIDsAndNames()
	}
	return out, nil
}

func (c *Client) listChannelIDsAndNames() ([]ChannelInfo, error) {
	raw, _, err := c.request(http.MethodGet, "/api/channels/idsAndNames", nil, nil, true)
	if err != nil {
		return nil, err
	}
	return parseIDsAndNames(raw)
}

func (c *Client) GetMetaDataColumns(channelID string) ([]MetaDataColumn, error) {
	path := fmt.Sprintf("/api/channels/%s/metaDataColumns", url.PathEscape(channelID))
	raw, _, err := c.request(http.MethodGet, path, nil, nil, true)
	if err != nil {
		return nil, err
	}
	return parseMetaDataColumns(raw)
}

func (c *Client) messageQuery(filter MessageFilter, offset, limit int, includeContent bool) url.Values {
	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	q := url.Values{}
	q.Set("includeContent", strconv.FormatBool(includeContent))
	q.Set("offset", strconv.Itoa(offset))
	q.Set("limit", strconv.Itoa(limit))
	if filter.MinMessageID != nil {
		q.Set("minMessageId", strconv.FormatInt(*filter.MinMessageID, 10))
	}
	if filter.MaxMessageID != nil {
		q.Set("maxMessageId", strconv.FormatInt(*filter.MaxMessageID, 10))
	}
	if filter.StartDate != "" {
		q.Set("startDate", filter.StartDate)
	}
	if filter.EndDate != "" {
		q.Set("endDate", filter.EndDate)
	}
	for _, st := range filter.Statuses {
		if st != "" {
			q.Add("status", st)
		}
	}
	if filter.Error != nil && *filter.Error {
		q.Set("error", "true")
	}
	for _, md := range filter.MetaDataSearch {
		if strings.TrimSpace(md.ColumnName) == "" || md.Value == nil || fmt.Sprint(md.Value) == "" {
			continue
		}
		op := md.Operator
		if op == "" {
			op = "CONTAINS"
		}
		clause := fmt.Sprintf("%s %s %s", md.ColumnName, op, fmt.Sprint(md.Value))
		if md.IgnoreCase {
			q.Add("metaDataCaseInsensitiveSearch", clause)
		} else {
			q.Add("metaDataSearch", clause)
		}
	}
	return q
}

func (c *Client) SearchMessages(channelID string, filter MessageFilter, offset, limit int, includeContent bool) ([]MessageSummary, error) {
	// This Mirth 4.5.2 instance returns HTTP 500 for POST .../messages/_search.
	// Use GET /messages with query filters instead (verified working).
	q := c.messageQuery(filter, offset, limit, includeContent)
	path := fmt.Sprintf("/api/channels/%s/messages", url.PathEscape(channelID))
	raw, _, err := c.request(http.MethodGet, path, q, nil, true)
	if err != nil {
		return nil, err
	}
	msgs, err := parseMessages(raw)
	if err != nil {
		return nil, err
	}
	return summarizeMessages(msgs), nil
}

func (c *Client) CountMessages(channelID string, filter MessageFilter) (int64, error) {
	q := c.messageQuery(filter, 0, 1, false)
	q.Del("includeContent")
	q.Del("offset")
	q.Del("limit")
	path := fmt.Sprintf("/api/channels/%s/messages/count", url.PathEscape(channelID))
	raw, _, err := c.request(http.MethodGet, path, q, nil, true)
	if err != nil {
		return 0, err
	}
	return parseInt64Value(raw)
}

func (c *Client) GetMessage(channelID string, messageID int64) (MessageDetail, error) {
	path := fmt.Sprintf("/api/channels/%s/messages/%d", url.PathEscape(channelID), messageID)
	raw, _, err := c.request(http.MethodGet, path, nil, nil, true)
	if err != nil {
		return MessageDetail{}, err
	}
	msg, err := parseSingleMessage(raw)
	if err != nil {
		return MessageDetail{}, err
	}
	return detailFromRaw(msg), nil
}

func (c *Client) MaxMessageID(channelID string) (int64, error) {
	path := fmt.Sprintf("/api/channels/%s/messages/maxMessageId", url.PathEscape(channelID))
	raw, _, err := c.request(http.MethodGet, path, nil, nil, true)
	if err != nil {
		return 0, err
	}
	return parseInt64Value(raw)
}

func (c *Client) FetchErrorMessages(channelID, channelName string, minMessageID int64, limit int) ([]ErrorMessage, error) {
	if limit <= 0 {
		limit = 50
	}
	q := url.Values{}
	q.Set("status", "ERROR")
	q.Set("includeContent", "false")
	q.Set("offset", "0")
	q.Set("limit", strconv.Itoa(limit))
	if minMessageID > 0 {
		q.Set("minMessageId", strconv.FormatInt(minMessageID+1, 10))
	}
	path := fmt.Sprintf("/api/channels/%s/messages", url.PathEscape(channelID))
	raw, _, err := c.request(http.MethodGet, path, q, nil, true)
	if err != nil {
		return nil, err
	}
	msgs, err := parseMessages(raw)
	if err != nil {
		return nil, err
	}
	out := make([]ErrorMessage, 0)
	for _, msg := range msgs {
		if msg.MessageID <= minMessageID {
			continue
		}
		meta := map[string]string{}
		for _, conn := range msg.Connectors {
			for k, v := range conn.MetaData {
				if strings.TrimSpace(v) != "" {
					meta[k] = v
				}
			}
		}
		for _, conn := range msg.Connectors {
			if !strings.EqualFold(conn.Status, "ERROR") {
				continue
			}
			errText := firstNonEmpty(conn.ProcessingError, conn.ResponseError, conn.PostProcessorError)
			name := channelName
			if conn.ChannelName != "" {
				name = conn.ChannelName
			}
			received := conn.ReceivedDate
			if received == "" {
				received = msg.ReceivedDate
			}
			// Prefer connector-local metadata, fall back to merged message map.
			connMeta := map[string]string{}
			for k, v := range meta {
				connMeta[k] = v
			}
			for k, v := range conn.MetaData {
				if strings.TrimSpace(v) != "" {
					connMeta[k] = v
				}
			}
			out = append(out, ErrorMessage{
				ChannelID:     channelID,
				ChannelName:   name,
				MessageID:     msg.MessageID,
				ConnectorName: conn.ConnectorName,
				ErrorText:     truncate(errText, 800),
				ReceivedDate:  received,
				MetaData:      connMeta,
			})
		}
	}
	return out, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func isXML(b []byte) bool {
	s := strings.TrimSpace(string(b))
	return strings.HasPrefix(s, "<")
}

// Flexible JSON helpers for Mirth's map/list wrappers.

func parseDashboardStatuses(raw []byte) ([]dashboardStatus, error) {
	if isXML(raw) {
		var list struct {
			XMLName xml.Name `xml:"list"`
			Items   []struct {
				ChannelID  string `xml:"channelId"`
				Name       string `xml:"name"`
				State      string `xml:"state"`
				StatusType string `xml:"statusType"`
				Queued     int64  `xml:"queued"`
				Statistics struct {
					Entries []struct {
						String string `xml:"string"`
						Long   int64  `xml:"long"`
					} `xml:"entry"`
				} `xml:"statistics"`
			} `xml:"dashboardStatus"`
		}
		if err := xml.Unmarshal(raw, &list); err != nil {
			return nil, err
		}
		out := make([]dashboardStatus, 0, len(list.Items))
		for _, it := range list.Items {
			stats := map[string]int64{}
			for _, e := range it.Statistics.Entries {
				stats[strings.ToUpper(e.String)] = e.Long
			}
			out = append(out, dashboardStatus{
				ChannelID:  it.ChannelID,
				Name:       it.Name,
				State:      it.State,
				StatusType: it.StatusType,
				Queued:     it.Queued,
				Statistics: stats,
			})
		}
		return out, nil
	}

	var asList struct {
		List struct {
			DashboardStatus json.RawMessage `json:"dashboardStatus"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &asList); err == nil && len(asList.List.DashboardStatus) > 0 {
		items, err := decodeStatusArray(asList.List.DashboardStatus)
		if err == nil {
			return items, nil
		}
	}
	return decodeStatusArray(raw)
}

func decodeStatusArray(raw []byte) ([]dashboardStatus, error) {
	var arr []dashboardStatus
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr, nil
	}
	var one dashboardStatus
	if err := json.Unmarshal(raw, &one); err == nil && one.ChannelID != "" {
		return []dashboardStatus{one}, nil
	}
	return nil, fmt.Errorf("unable to parse channel statuses: %s", truncate(string(raw), 200))
}

func normalizeStats(v any) map[string]int64 {
	out := map[string]int64{}
	switch t := v.(type) {
	case map[string]int64:
		for k, val := range t {
			out[strings.ToUpper(k)] = val
		}
	case map[string]any:
		// plain map or Mirth entry wrapper
		if entries, ok := t["entry"]; ok {
			for k, val := range parseEntryStats(entries) {
				out[k] = val
			}
			return out
		}
		for k, val := range t {
			out[strings.ToUpper(k)] = toInt64(val)
		}
	case nil:
		return out
	default:
		// try remarshal
		b, _ := json.Marshal(v)
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			return normalizeStats(m)
		}
	}
	return out
}

func parseEntryStats(entries any) map[string]int64 {
	out := map[string]int64{}
	arr, ok := entries.([]any)
	if !ok {
		// single entry object
		if m, ok := entries.(map[string]any); ok {
			arr = []any{m}
		} else {
			return out
		}
	}
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		key := ""
		var val int64
		if s, ok := m["string"].(string); ok {
			key = s
		}
		if stringsArr, ok := m["string"].([]any); ok && len(stringsArr) > 0 {
			key = fmt.Sprint(stringsArr[0])
			if len(stringsArr) > 1 {
				val = toInt64(stringsArr[1])
			}
		}
		// Mirth JSON: {"com.mirth.connect.donkey.model.message.Status":"RECEIVED","long":156}
		if key == "" {
			for k, v := range m {
				lk := strings.ToLower(k)
				if strings.Contains(lk, "status") {
					key = fmt.Sprint(v)
					break
				}
			}
		}
		if l, ok := m["long"]; ok {
			val = toInt64(l)
		} else if l, ok := m["int"]; ok {
			val = toInt64(l)
		} else if l, ok := m["big-decimal"]; ok {
			val = toInt64(l)
		}
		if key != "" {
			out[strings.ToUpper(key)] = val
		}
	}
	return out
}

func toInt64(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	case json.Number:
		n, _ := t.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	default:
		n, _ := strconv.ParseInt(fmt.Sprint(v), 10, 64)
		return n
	}
}

func parseIDsAndNames(raw []byte) ([]ChannelInfo, error) {
	if isXML(raw) {
		var doc struct {
			Entries []struct {
				Strings []string `xml:"string"`
			} `xml:"entry"`
		}
		if err := xml.Unmarshal(raw, &doc); err != nil {
			return nil, err
		}
		out := make([]ChannelInfo, 0, len(doc.Entries))
		for _, e := range doc.Entries {
			if len(e.Strings) >= 2 {
				out = append(out, ChannelInfo{ID: e.Strings[0], Name: e.Strings[1]})
			}
		}
		return out, nil
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err == nil && len(m) > 0 {
		out := make([]ChannelInfo, 0, len(m))
		for id, name := range m {
			out = append(out, ChannelInfo{ID: id, Name: name})
		}
		return out, nil
	}
	var wrapped struct {
		Map struct {
			Entry []struct {
				String []string `json:"string"`
			} `json:"entry"`
		} `json:"map"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil {
		out := make([]ChannelInfo, 0)
		for _, e := range wrapped.Map.Entry {
			if len(e.String) >= 2 {
				out = append(out, ChannelInfo{ID: e.String[0], Name: e.String[1]})
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	// entry array at root map style
	var rootEntries struct {
		Entry []struct {
			String []string `json:"string"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(raw, &rootEntries); err == nil && len(rootEntries.Entry) > 0 {
		out := make([]ChannelInfo, 0)
		for _, e := range rootEntries.Entry {
			if len(e.String) >= 2 {
				out = append(out, ChannelInfo{ID: e.String[0], Name: e.String[1]})
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("unexpected idsAndNames response")
}

func parseChannelList(raw []byte) ([]ChannelInfo, error) {
	var list struct {
		List struct {
			Channel []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"channel"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &list); err == nil && len(list.List.Channel) > 0 {
		out := make([]ChannelInfo, 0, len(list.List.Channel))
		for _, ch := range list.List.Channel {
			out = append(out, ChannelInfo{ID: ch.ID, Name: ch.Name})
		}
		return out, nil
	}
	var arr []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &arr); err == nil {
		out := make([]ChannelInfo, 0, len(arr))
		for _, ch := range arr {
			out = append(out, ChannelInfo{ID: ch.ID, Name: ch.Name})
		}
		return out, nil
	}
	return nil, fmt.Errorf("unable to parse channels")
}

func parseMetaDataColumns(raw []byte) ([]MetaDataColumn, error) {
	if isXML(raw) {
		var list struct {
			Items []MetaDataColumn `xml:"metaDataColumn"`
		}
		if err := xml.Unmarshal(raw, &list); err != nil {
			return nil, err
		}
		return list.Items, nil
	}
	var list struct {
		List struct {
			MetaDataColumn []MetaDataColumn `json:"metaDataColumn"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &list); err == nil && len(list.List.MetaDataColumn) > 0 {
		return list.List.MetaDataColumn, nil
	}
	var arr []MetaDataColumn
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr, nil
	}
	var one MetaDataColumn
	if err := json.Unmarshal(raw, &one); err == nil && one.Name != "" {
		return []MetaDataColumn{one}, nil
	}
	return []MetaDataColumn{}, nil
}

type parsedMessage struct {
	MessageID   int64
	ChannelID   string
	ReceivedDate string
	Connectors  []parsedConnector
}

type parsedConnector struct {
	MetaDataID         int
	ChannelID          string
	ChannelName        string
	ConnectorName      string
	ReceivedDate       string
	Status             string
	MetaData           map[string]string
	ProcessingError    string
	ResponseError      string
	PostProcessorError string
}

func parseMessages(raw []byte) ([]parsedMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []parsedMessage{}, nil
	}
	if isXML(raw) {
		return parseMessagesXML(raw)
	}
	var list struct {
		List struct {
			Message json.RawMessage `json:"message"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &list); err == nil {
		if len(list.List.Message) == 0 || string(list.List.Message) == "null" {
			return []parsedMessage{}, nil
		}
		return decodeMessageArray(list.List.Message)
	}
	return decodeMessageArray(raw)
}

func parseSingleMessage(raw []byte) (parsedMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return parsedMessage{}, fmt.Errorf("empty message response")
	}
	// Mirth often returns {"message":{...}} for GET by id.
	var wrapped struct {
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && len(wrapped.Message) > 0 && string(wrapped.Message) != "null" {
		var obj map[string]any
		if err := json.Unmarshal(wrapped.Message, &obj); err == nil {
			return mapToMessage(obj), nil
		}
		msgs, err := decodeMessageArray(wrapped.Message)
		if err == nil && len(msgs) > 0 {
			return msgs[0], nil
		}
	}
	msgs, err := parseMessages(raw)
	if err != nil {
		return parsedMessage{}, err
	}
	if len(msgs) == 0 {
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err == nil {
			if _, hasID := obj["messageId"]; hasID {
				return mapToMessage(obj), nil
			}
		}
		return parsedMessage{}, fmt.Errorf("message not found")
	}
	return msgs[0], nil
}

func decodeMessageArray(raw []byte) ([]parsedMessage, error) {
	var objs []map[string]any
	if err := json.Unmarshal(raw, &objs); err != nil {
		var one map[string]any
		if err2 := json.Unmarshal(raw, &one); err2 != nil {
			return nil, fmt.Errorf("decode messages: %w", err)
		}
		objs = []map[string]any{one}
	}
	out := make([]parsedMessage, 0, len(objs))
	for _, o := range objs {
		out = append(out, mapToMessage(o))
	}
	return out, nil
}

func mapToMessage(o map[string]any) parsedMessage {
	msg := parsedMessage{
		MessageID:    toInt64(o["messageId"]),
		ChannelID:    fmt.Sprint(nilToEmpty(o["channelId"])),
		ReceivedDate: formatAnyDate(o["receivedDate"]),
		Connectors:   []parsedConnector{},
	}
	if msg.ChannelID == "<nil>" {
		msg.ChannelID = ""
	}
	connRaw := o["connectorMessages"]
	for _, c := range extractConnectors(connRaw) {
		msg.Connectors = append(msg.Connectors, c)
	}
	return msg
}

func extractConnectors(v any) []parsedConnector {
	out := []parsedConnector{}
	switch t := v.(type) {
	case map[string]any:
		if entry, ok := t["entry"]; ok {
			return extractConnectors(entry)
		}
		// keyed by metadata id as string keys
		for k, val := range t {
			if k == "@class" {
				continue
			}
			if m, ok := val.(map[string]any); ok {
				if cm, ok := m["connectorMessage"].(map[string]any); ok {
					out = append(out, mapToConnector(cm))
				} else if _, hasStatus := m["status"]; hasStatus {
					out = append(out, mapToConnector(m))
				}
			}
		}
	case []any:
		for _, item := range t {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if cm, ok := m["connectorMessage"].(map[string]any); ok {
				out = append(out, mapToConnector(cm))
				continue
			}
			if _, hasStatus := m["status"]; hasStatus {
				out = append(out, mapToConnector(m))
			}
		}
	}
	return out
}

func mapToConnector(m map[string]any) parsedConnector {
	c := parsedConnector{
		MetaDataID:    int(toInt64(m["metaDataId"])),
		ChannelID:     fmt.Sprint(nilToEmpty(m["channelId"])),
		ChannelName:   fmt.Sprint(nilToEmpty(m["channelName"])),
		ConnectorName: fmt.Sprint(nilToEmpty(m["connectorName"])),
		ReceivedDate:  formatAnyDate(m["receivedDate"]),
		Status:        fmt.Sprint(nilToEmpty(m["status"])),
		MetaData:      map[string]string{},
	}
	if c.ChannelID == "<nil>" {
		c.ChannelID = ""
	}
	if md, ok := m["metaDataMap"]; ok {
		c.MetaData = flattenMeta(md)
	}
	c.ProcessingError = errorContentString(m["processingErrorContent"])
	c.ResponseError = errorContentString(m["responseErrorContent"])
	c.PostProcessorError = errorContentString(m["postProcessorErrorContent"])
	return c
}

func errorContentString(v any) string {
	m, ok := v.(map[string]any)
	if !ok || m == nil {
		return ""
	}
	return fmt.Sprint(nilToEmpty(m["content"]))
}

func flattenMeta(v any) map[string]string {
	out := map[string]string{}
	switch t := v.(type) {
	case map[string]any:
		if entry, ok := t["entry"]; ok {
			for k, val := range flattenMetaEntries(entry) {
				out[k] = val
			}
			return out
		}
		for k, val := range t {
			if k == "@class" {
				continue
			}
			out[k] = formatMetaValue(val)
		}
	}
	return out
}

func formatMetaValue(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		// JSON numbers decode as float64; avoid scientific notation for IDs like MRN/ORDERNUMBER.
		if t == float64(int64(t)) && t >= -1e15 && t <= 1e15 {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return strconv.FormatInt(i, 10)
		}
		if f, err := t.Float64(); err == nil {
			return formatMetaValue(f)
		}
		return t.String()
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case bool:
		return strconv.FormatBool(t)
	default:
		s := fmt.Sprint(t)
		if s == "<nil>" {
			return ""
		}
		return s
	}
}

func flattenMetaEntries(entries any) map[string]string {
	out := map[string]string{}
	arr, ok := entries.([]any)
	if !ok {
		if m, ok := entries.(map[string]any); ok {
			arr = []any{m}
		} else {
			return out
		}
	}
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		// Mirth form: {"string": ["ORDERNUMBER", 560302506]}
		if strs, ok := m["string"].([]any); ok && len(strs) >= 2 {
			key := fmt.Sprint(strs[0])
			out[key] = formatMetaValue(strs[1])
			continue
		}
		key := ""
		if s, ok := m["string"].(string); ok {
			key = s
		}
		if key == "" {
			continue
		}
		found := false
		for _, vk := range []string{"boolean", "long", "int", "double", "big-decimal", "date", "null"} {
			if val, ok := m[vk]; ok {
				out[key] = formatMetaValue(val)
				found = true
				break
			}
		}
		if !found {
			out[key] = ""
		}
	}
	return out
}

func parseMessagesXML(raw []byte) ([]parsedMessage, error) {
	var list struct {
		Messages []struct {
			MessageID string `xml:"messageId"`
			ChannelID string `xml:"channelId"`
			Connectors struct {
				Entries []struct {
					Connector struct {
						MetaDataID    string `xml:"metaDataId"`
						ChannelName   string `xml:"channelName"`
						ConnectorName string `xml:"connectorName"`
						Status        string `xml:"status"`
						ReceivedDate  string `xml:"receivedDate"`
						ProcErr       string `xml:"processingErrorContent>content"`
						RespErr       string `xml:"responseErrorContent>content"`
					} `xml:"connectorMessage"`
				} `xml:"entry"`
			} `xml:"connectorMessages"`
		} `xml:"message"`
	}
	if err := xml.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	out := make([]parsedMessage, 0, len(list.Messages))
	for _, m := range list.Messages {
		pm := parsedMessage{
			MessageID:  toInt64(m.MessageID),
			ChannelID:  m.ChannelID,
			Connectors: []parsedConnector{},
		}
		for _, e := range m.Connectors.Entries {
			pm.Connectors = append(pm.Connectors, parsedConnector{
				MetaDataID:      int(toInt64(e.Connector.MetaDataID)),
				ChannelName:     e.Connector.ChannelName,
				ConnectorName:   e.Connector.ConnectorName,
				Status:          e.Connector.Status,
				ReceivedDate:    e.Connector.ReceivedDate,
				ProcessingError: e.Connector.ProcErr,
				ResponseError:   e.Connector.RespErr,
				MetaData:        map[string]string{},
			})
		}
		out = append(out, pm)
	}
	return out, nil
}

func parseInt64Value(raw []byte) (int64, error) {
	s := strings.TrimSpace(string(raw))
	if isXML(raw) {
		var doc struct {
			Long string `xml:",chardata"`
		}
		_ = xml.Unmarshal(raw, &doc)
		if n, err := strconv.ParseInt(strings.TrimSpace(doc.Long), 10, 64); err == nil {
			return n, nil
		}
		// try <long>123</long>
		var longDoc struct {
			Value string `xml:"long"`
		}
		if xml.Unmarshal(raw, &longDoc) == nil {
			if n, err := strconv.ParseInt(strings.TrimSpace(longDoc.Value), 10, 64); err == nil {
				return n, nil
			}
		}
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, nil
	}
	var sVal string
	if err := json.Unmarshal(raw, &sVal); err == nil {
		return strconv.ParseInt(sVal, 10, 64)
	}
	var wrapped map[string]any
	if err := json.Unmarshal(raw, &wrapped); err == nil {
		for _, v := range wrapped {
			return toInt64(v), nil
		}
	}
	return strconv.ParseInt(strings.Trim(s, "\" \n\r"), 10, 64)
}

func nilToEmpty(v any) any {
	if v == nil {
		return ""
	}
	return v
}

func summarizeMessages(msgs []parsedMessage) []MessageSummary {
	out := make([]MessageSummary, 0, len(msgs))
	for _, msg := range msgs {
		status := ""
		meta := map[string]string{}
		received := msg.ReceivedDate
		channelName := ""
		for _, conn := range msg.Connectors {
			if status == "" {
				status = conn.Status
			}
			if strings.EqualFold(conn.Status, "ERROR") {
				status = "ERROR"
			}
			if received == "" {
				received = conn.ReceivedDate
			}
			if channelName == "" {
				channelName = conn.ChannelName
			}
			for k, v := range conn.MetaData {
				meta[k] = v
			}
		}
		out = append(out, MessageSummary{
			MessageID:    msg.MessageID,
			ReceivedDate: received,
			Status:       status,
			MetaData:     meta,
			ChannelID:    msg.ChannelID,
			ChannelName:  channelName,
		})
	}
	return out
}

func detailFromRaw(msg parsedMessage) MessageDetail {
	detail := MessageDetail{
		MessageID:    msg.MessageID,
		ChannelID:    msg.ChannelID,
		ReceivedDate: msg.ReceivedDate,
		MetaData:     map[string]string{},
		Connectors:   make([]ConnectorDetail, 0),
	}
	for _, conn := range msg.Connectors {
		cd := ConnectorDetail{
			MetaDataID:      conn.MetaDataID,
			ConnectorName:   conn.ConnectorName,
			Status:          conn.Status,
			ReceivedDate:    conn.ReceivedDate,
			MetaData:        conn.MetaData,
			ProcessingError: conn.ProcessingError,
			ResponseError:   conn.ResponseError,
		}
		for k, v := range conn.MetaData {
			detail.MetaData[k] = v
		}
		if detail.ChannelName == "" {
			detail.ChannelName = conn.ChannelName
		}
		if detail.Status == "" || strings.EqualFold(conn.Status, "ERROR") {
			detail.Status = conn.Status
		}
		if detail.ReceivedDate == "" {
			detail.ReceivedDate = cd.ReceivedDate
		}
		detail.Connectors = append(detail.Connectors, cd)
	}
	return detail
}

func formatAnyDate(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return time.UnixMilli(int64(t)).Format(time.RFC3339)
	case map[string]any:
		if timeStr, ok := t["time"].(string); ok {
			return timeStr
		}
		if millis, ok := t["time"].(float64); ok {
			return time.UnixMilli(int64(millis)).Format(time.RFC3339)
		}
	}
	s := fmt.Sprint(v)
	if s == "<nil>" {
		return ""
	}
	return s
}
