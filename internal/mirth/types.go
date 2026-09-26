package mirth

import "time"

type ConnectionMode string

const (
	ModeConnected   ConnectionMode = "connected"
	ModeUnreachable ConnectionMode = "unreachable"
)

type ChannelStats struct {
	ChannelID string `json:"channelId"`
	Name      string `json:"name"`
	State     string `json:"state"`
	Received  int64  `json:"received"`
	Sent      int64  `json:"sent"`
	Error     int64  `json:"error"`
	Filtered  int64  `json:"filtered"`
	Queued    int64  `json:"queued"`
}

type DashboardSummary struct {
	Mode            ConnectionMode `json:"mode"`
	ChannelsStarted int            `json:"channelsStarted"`
	ChannelCount    int            `json:"channelCount"`
	Received        int64          `json:"received"`
	Sent            int64          `json:"sent"`
	Error           int64          `json:"error"`
	Filtered        int64          `json:"filtered"`
	Queued          int64          `json:"queued"`
	Channels        []ChannelStats `json:"channels"`
	RefreshedAt     time.Time      `json:"refreshedAt"`
	ErrorMessage    string         `json:"errorMessage,omitempty"`
}

type MetaDataColumn struct {
	Name        string `json:"name" xml:"name"`
	Type        string `json:"type" xml:"type"`
	MappingName string `json:"mappingName,omitempty" xml:"mappingName"`
}

type ChannelInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type MessageFilter struct {
	MinMessageID   *int64                  `json:"minMessageId,omitempty"`
	MaxMessageID   *int64                  `json:"maxMessageId,omitempty"`
	StartDate      string                  `json:"startDate,omitempty"`
	EndDate        string                  `json:"endDate,omitempty"`
	Statuses       []string                `json:"statuses,omitempty"`
	MetaDataSearch []MetaDataSearchElement `json:"metaDataSearch,omitempty"`
	Error          *bool                   `json:"error,omitempty"`
}

type MetaDataSearchElement struct {
	ColumnName string `json:"columnName"`
	Operator   string `json:"operator"`
	Value      any    `json:"value"`
	IgnoreCase bool   `json:"ignoreCase,omitempty"`
}

type MessageSummary struct {
	MessageID    int64             `json:"messageId"`
	ReceivedDate string            `json:"receivedDate"`
	Status       string            `json:"status"`
	MetaData     map[string]string `json:"metaData"`
	ChannelID    string            `json:"channelId"`
	ChannelName  string            `json:"channelName,omitempty"`
}

type ConnectorDetail struct {
	MetaDataID      int               `json:"metaDataId"`
	ConnectorName   string            `json:"connectorName"`
	Status          string            `json:"status"`
	ReceivedDate    string            `json:"receivedDate"`
	MetaData        map[string]string `json:"metaData"`
	ProcessingError string            `json:"processingError,omitempty"`
	ResponseError   string            `json:"responseError,omitempty"`
}

type MessageDetail struct {
	MessageID    int64             `json:"messageId"`
	ChannelID    string            `json:"channelId"`
	ChannelName  string            `json:"channelName"`
	ReceivedDate string            `json:"receivedDate"`
	Status       string            `json:"status"`
	MetaData     map[string]string `json:"metaData"`
	Connectors   []ConnectorDetail `json:"connectors"`
}

type ErrorMessage struct {
	ChannelID     string
	ChannelName   string
	MessageID     int64
	ConnectorName string
	ErrorText     string
	ReceivedDate  string
	MetaData      map[string]string
}

type dashboardStatus struct {
	ChannelID  string `json:"channelId"`
	Name       string `json:"name"`
	State      string `json:"state"`
	Statistics any    `json:"statistics"`
	Queued     int64  `json:"queued"`
	StatusType string `json:"statusType"`
	MetaDataID *int   `json:"metaDataId"`
}
