package main

import "time"

const (
	fileExtension  = ".excalidraw"
	libraryName    = "library.excalidrawlib"
	localSourceID  = "local"
	emptyBoardJSON = `{
  "type": "excalidraw",
  "version": 2,
  "source": "excalibur",
  "elements": [],
  "appState": { "gridSize": 20, "viewBackgroundColor": "#ffffff" },
  "files": {}
}`
)

type Viewport struct {
	ScrollX float64 `json:"scrollX"`
	ScrollY float64 `json:"scrollY"`
	Zoom    float64 `json:"zoom"`
}

type NatsServerConfig struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	URL             string   `json:"url"`
	Username        string   `json:"username,omitempty"`
	Password        string   `json:"password,omitempty"`
	CredentialsFile string   `json:"credentialsFile,omitempty"`
	Buckets         []string `json:"buckets"`
}

// NatsServerSettings is safe to expose to the webview. Passwords remain on the
// native side; the UI only needs to know whether one is already saved.
type NatsServerSettings struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	URL             string   `json:"url"`
	Username        string   `json:"username"`
	CredentialsFile string   `json:"credentialsFile"`
	Buckets         []string `json:"buckets"`
	HasPassword     bool     `json:"hasPassword"`
}

type AIConfig struct {
	Enabled        bool   `json:"enabled"`
	BaseURL        string `json:"baseUrl"`
	Model          string `json:"model"`
	APIKey         string `json:"apiKey,omitempty"`
	EnableThinking bool   `json:"-"`
}

// AISettings is safe to expose to the webview: the stored API key is never
// returned, only whether one has been configured.
type AISettings struct {
	Enabled   bool   `json:"enabled"`
	BaseURL   string `json:"baseUrl"`
	Model     string `json:"model"`
	HasAPIKey bool   `json:"hasApiKey"`
}

type AISettingsUpdate struct {
	Enabled     bool   `json:"enabled"`
	BaseURL     string `json:"baseUrl"`
	Model       string `json:"model"`
	APIKey      string `json:"apiKey"`
	ClearAPIKey bool   `json:"clearApiKey"`
}

type AIChatMessage struct {
	Role     string `json:"role"`
	Content  string `json:"content"`
	Thinking string `json:"thinking,omitempty"`
}

// AIModel separates the gateway's friendly identifier from the exact model
// name required by the chat-completions dataplane.
type AIModel struct {
	ID       string `json:"id"`
	Model    string `json:"model"`
	Thinking bool   `json:"thinking"`
}

type Preferences struct {
	BoardsFolder     string              `json:"boardsFolder,omitempty"`
	LastBoard        string              `json:"lastBoard,omitempty"`
	Viewports        map[string]Viewport `json:"viewports"`
	SortOrder        string              `json:"sortOrder"`
	CanvasAppearance string              `json:"canvasAppearance"`
	ApplicationZoom  float64             `json:"applicationZoom"`
	ShowThumbnails   *bool               `json:"showThumbnails,omitempty"`
	CollabNickname   string              `json:"collaborationNickname,omitempty"`
	CollabColor      string              `json:"collaborationColor,omitempty"`
	CollabAvatar     string              `json:"collaborationAvatar,omitempty"`
	CollabAvatarOnly bool                `json:"collaborationAvatarOnly,omitempty"`
	NatsServers      []NatsServerConfig  `json:"natsServers"`
	ActiveSource     string              `json:"activeSource,omitempty"`
	ActiveGroup      string              `json:"activeGroup,omitempty"`
	AI               AIConfig            `json:"ai"`
}

type Board struct {
	ID         string
	Name       string
	DocumentID string
	Modified   time.Time
	Size       uint64
	IsLink     bool
	Target     string
	Path       string
}

type BoardDetails struct {
	Texts     []string
	Thumbnail string
}

type BoardInfo struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	DocumentID string   `json:"documentId,omitempty"`
	Modified   int64    `json:"modified"`
	IsLink     bool     `json:"isLink"`
	Target     *string  `json:"target"`
	Texts      []string `json:"texts"`
	Thumbnail  *string  `json:"thumbnail"`
}

type CollabJoinResult struct {
	Snapshot *string `json:"snapshot"`
	// JetStream sequences are decimal strings because JavaScript numbers lose
	// integer precision above 2^53-1.
	SnapshotSequence string `json:"snapshotSequence"`
	// Last retained update for this document when the consumer was created.
	// The renderer does not report catch-up before this sequence is applied.
	ReplayThroughSequence string `json:"replayThroughSequence"`
	// Durable local updates that have not yet received a JetStream publish ACK.
	PendingLocalUpdates int `json:"pendingLocalUpdates"`
}

type Notice struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Message string `json:"message"`
}

type GroupInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type SourceInfo struct {
	ID     string      `json:"id"`
	Name   string      `json:"name"`
	Kind   string      `json:"kind"`
	Groups []GroupInfo `json:"groups"`
}

type StoreSnapshot struct {
	Folder           string               `json:"folder"`
	Boards           []BoardInfo          `json:"boards"`
	Selection        *string              `json:"selection"`
	SortOrder        string               `json:"sortOrder"`
	ShowThumbnails   bool                 `json:"showThumbnails"`
	CanvasAppearance string               `json:"canvasAppearance"`
	ApplicationZoom  float64              `json:"applicationZoom"`
	CollabNickname   string               `json:"collaborationNickname"`
	CollabColor      string               `json:"collaborationColor"`
	CollabAvatar     string               `json:"collaborationAvatar"`
	CollabAvatarOnly bool                 `json:"collaborationAvatarOnly"`
	Alerts           []Notice             `json:"alerts"`
	IsAlertPresented bool                 `json:"isAlertPresented"`
	Sources          []SourceInfo         `json:"sources"`
	NatsServers      []NatsServerSettings `json:"natsServers"`
	ActiveSource     string               `json:"activeSource"`
	ActiveGroup      string               `json:"activeGroup"`
	AI               AISettings           `json:"ai"`
}

type PickFileResult struct {
	Name    string `json:"name"`
	DataURL string `json:"dataUrl"`
}
