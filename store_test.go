package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestLockKeyIsValidAndDeterministic(t *testing.T) {
	bucket := "team bucket/日本語"
	board := "Roadmap 2026 #1.excalidraw"
	wantHash := sha256.Sum256([]byte(bucket + "\x00" + board))
	want := fmt.Sprintf("board.%x", wantHash)

	got := lockKey(bucket, board)
	if got != want {
		t.Fatalf("lockKey() = %q, want %q", got, want)
	}
	if !regexp.MustCompile(`^[-/_=.a-zA-Z0-9]+$`).MatchString(got) {
		t.Fatalf("lockKey() produced an invalid NATS KV key: %q", got)
	}
}

func TestLockKeySeparatesBucketAndBoard(t *testing.T) {
	if lockKey("a", "b/c") == lockKey("a/b", "c") {
		t.Fatal("lockKey() collided for distinct bucket/board tuples")
	}
}

func testStore(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()
	store, err := newStore(filepath.Join(root, "config", "prefs.json"), filepath.Join(root, "boards"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.close)
	return store
}

func TestPreferencesPathMigratesLegacyName(t *testing.T) {
	config := t.TempDir()
	legacyPath := filepath.Join(config, "excaliwails", "prefs.json")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	const prefs = `{"sortOrder":"date"}`
	if err := os.WriteFile(legacyPath, []byte(prefs), 0o600); err != nil {
		t.Fatal(err)
	}

	path := preferencesPath(config)
	if want := filepath.Join(config, "excalibur", "prefs.json"); path != want {
		t.Fatalf("preferencesPath() = %q, want %q", path, want)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != prefs {
		t.Fatalf("migrated preferences = %q, %v", data, err)
	}
}

func TestLocalBoardLifecycle(t *testing.T) {
	store := testStore(t)
	if err := store.createBoard("Plan"); err != nil {
		t.Fatal(err)
	}
	if store.selection != "Plan.excalidraw" {
		t.Fatalf("selection = %q", store.selection)
	}
	scene := `{"type":"excalidraw","elements":[{"text":"hello"}]}`
	if err := store.saveBoard("Plan", scene); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(store.localFolder, "Plan.excalidraw")); err != nil || string(got) != scene {
		t.Fatalf("read = %q, %v", got, err)
	}
	if err := store.renameBoard("Plan.excalidraw", "Roadmap"); err != nil {
		t.Fatal(err)
	}
	if store.board("Roadmap.excalidraw") == nil {
		t.Fatal("renamed board missing")
	}
	if err := store.duplicateBoard("Roadmap.excalidraw"); err != nil {
		t.Fatal(err)
	}
	if store.board("Roadmap copy.excalidraw") == nil {
		t.Fatal("duplicate missing")
	}
}

func TestValidationAndSearchText(t *testing.T) {
	store := testStore(t)
	if got := store.nameProblem("a/b", ""); got == "" {
		t.Fatal("slash should be rejected")
	}
	data := []byte(`{"elements":[{"originalText":"Alpha"},{"text":"Beta"},{"text":"Beta"},{"text":"gone","isDeleted":true}]}`)
	if got, want := searchableTexts(data), []string{"Alpha", "Beta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestDataURLAndCollaborationSubject(t *testing.T) {
	got, err := decodeDataURL("data:text/plain,hello%20world")
	if err != nil || string(got) != "hello world" {
		t.Fatalf("decode = %q, %v", got, err)
	}
	if got := collabSubject("team.bucket", "A board.excalidraw"); got != "wb.collab.team-bucket.A-board-excalidraw" {
		t.Fatalf("subject = %q", got)
	}
}

func TestNormalizeCollaborationNickname(t *testing.T) {
	if got, err := normalizeCollaborationNickname("  Merlin  "); err != nil || got != "Merlin" {
		t.Fatalf("normalized nickname = %q, %v", got, err)
	}
	if got, err := normalizeCollaborationNickname("   "); err != nil || got != "" {
		t.Fatalf("blank nickname = %q, %v", got, err)
	}
	if _, err := normalizeCollaborationNickname("line\nbreak"); err == nil {
		t.Fatal("nickname containing a control character should be rejected")
	}
	if _, err := normalizeCollaborationNickname(strings.Repeat("x", maxCollaborationNicknameLength+1)); err == nil {
		t.Fatal("overlong nickname should be rejected")
	}
	if got, err := normalizeCollaborationNickname(strings.Repeat("🗡", maxCollaborationNicknameLength)); err != nil || got == "" {
		t.Fatalf("rune-length nickname = %q, %v", got, err)
	}
}

func TestSetCollaborationNicknamePersists(t *testing.T) {
	store := testStore(t)
	app := newApp()
	app.store = store
	if err := app.SetCollaborationNickname("  Merlin  "); err != nil {
		t.Fatal(err)
	}
	if got := store.snapshot().CollabNickname; got != "Merlin" {
		t.Fatalf("snapshot nickname = %q", got)
	}
	data, err := os.ReadFile(store.prefsPath)
	if err != nil {
		t.Fatal(err)
	}
	var prefs Preferences
	if err = json.Unmarshal(data, &prefs); err != nil {
		t.Fatal(err)
	}
	if prefs.CollabNickname != "Merlin" {
		t.Fatalf("persisted nickname = %q", prefs.CollabNickname)
	}
}

func TestNormalizeNatsServerConfigCredentialsFile(t *testing.T) {
	credentials := filepath.Join(t.TempDir(), "operator.creds")
	if err := os.WriteFile(credentials, []byte("test credentials"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := normalizeNatsServerConfig(NatsServerConfig{
		ID:              " server ",
		Name:            " Secure NATS ",
		URL:             " nats://example.test:4222 ",
		Username:        " ignored ",
		Password:        "ignored",
		CredentialsFile: " " + credentials + " ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.ID != "server" || config.Name != "Secure NATS" || config.URL != "nats://example.test:4222" {
		t.Fatalf("server fields were not normalized: %#v", config)
	}
	if config.CredentialsFile != credentials {
		t.Fatalf("credentials file = %q, want %q", config.CredentialsFile, credentials)
	}
	if config.Username != "" || config.Password != "" {
		t.Fatalf("credentials file should replace username/password: %#v", config)
	}
}

func TestNormalizeNatsServerConfigRejectsInvalidCredentialsPath(t *testing.T) {
	base := NatsServerConfig{ID: "server", Name: "Secure NATS", URL: "nats://example.test:4222"}
	base.CredentialsFile = filepath.Join(t.TempDir(), "missing.creds")
	if _, err := normalizeNatsServerConfig(base); err == nil || !strings.Contains(err.Error(), "could not access NATS credentials file") {
		t.Fatalf("missing credentials error = %v", err)
	}
	base.CredentialsFile = t.TempDir()
	if _, err := normalizeNatsServerConfig(base); err == nil || !strings.Contains(err.Error(), "is not a file") {
		t.Fatalf("directory credentials error = %v", err)
	}
}

func TestNatsServerSnapshotRedactsPassword(t *testing.T) {
	store := testStore(t)
	store.prefs.NatsServers = []NatsServerConfig{{
		ID:       "private",
		Name:     "Private NATS",
		URL:      "nats://example.test:4222",
		Username: "merlin",
		Password: "sword-in-the-stone",
		Buckets:  []string{"boards"},
	}}

	snapshot := store.snapshot()
	if len(snapshot.NatsServers) != 1 {
		t.Fatalf("NATS settings count = %d, want 1", len(snapshot.NatsServers))
	}
	settings := snapshot.NatsServers[0]
	if settings.ID != "private" || settings.Username != "merlin" || !settings.HasPassword {
		t.Fatalf("NATS settings = %#v", settings)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "sword-in-the-stone") {
		t.Fatal("snapshot exposed the saved NATS password")
	}
}

func TestChatCompletionsURL(t *testing.T) {
	cases := map[string]string{
		"https://ai.example":                     "https://ai.example/v1/chat/completions",
		"https://ai.example/api/v1/":             "https://ai.example/api/v1/chat/completions",
		"https://ai.example/v1/chat/completions": "https://ai.example/v1/chat/completions",
	}
	for input, want := range cases {
		got, err := chatCompletionsURL(input)
		if err != nil || got != want {
			t.Errorf("chatCompletionsURL(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := chatCompletionsURL("file:///tmp/model"); err == nil {
		t.Fatal("non-HTTP URL should be rejected")
	}
}

func TestAIModelsURLUsesGatewayRoute(t *testing.T) {
	cases := map[string]string{
		"https://ai.example":                     "https://ai.example/gate/v1/models",
		"https://ai.example/api/v1/":             "https://ai.example/gate/v1/models",
		"https://ai.example/v1/chat/completions": "https://ai.example/gate/v1/models",
	}
	for input, want := range cases {
		got, err := aiModelsURL(input)
		if err != nil || got != want {
			t.Errorf("aiModelsURL(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
}

func TestAIChatRejectsBoardSwitch(t *testing.T) {
	store := testStore(t)
	if err := store.createBoard("First"); err != nil {
		t.Fatal(err)
	}
	if err := store.createBoard("Second"); err != nil {
		t.Fatal(err)
	}
	app := newApp()
	app.store = store

	if _, _, err := app.canvasDataForAI("First.excalidraw"); !errors.Is(err, errAIChatBoardChanged) {
		t.Fatalf("canvasDataForAI() error = %v, want board-changed error", err)
	}
	if _, err := app.AIChat([]AIChatMessage{{Role: "user", Content: "What is here?"}}, "test-model", "request", "First.excalidraw", false); !errors.Is(err, errAIChatBoardChanged) {
		t.Fatalf("AIChat() error = %v, want board-changed error", err)
	}
}

func TestRequestAIModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/gate/v1/models" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("authorization = %q", got)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"data":[{"id":"zeta-id","model":"zeta","reasoning":true},{"id":"alpha-id","model":"alpha","reasoning":true,"options":{"chat_template_kwargs":{"enable_thinking":false}}},{"id":"duplicate-id","model":"alpha"},{"id":"id-only"},{"model":""}]}`))
	}))
	defer server.Close()

	models, err := requestAIModels(server.Client(), server.URL+"/gate/v1/models", "secret")
	if err != nil {
		t.Fatal(err)
	}
	want := []AIModel{{ID: "alpha-id", Model: "alpha", Thinking: false}, {ID: "zeta-id", Model: "zeta", Thinking: true}}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %#v", models)
	}
	if !modelAvailable(models, "alpha") || modelAvailable(models, "alpha-id") || modelAvailable(models, "missing") {
		t.Fatalf("unexpected availability result for %#v", models)
	}
}

func TestOpenAIRequestIncludesCanvasToolAndOptionalKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("authorization = %q", got)
		}
		var payload struct {
			Model              string          `json:"model"`
			ChatTemplateKwargs map[string]bool `json:"chat_template_kwargs"`
			Tools              []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(writer, "invalid request", http.StatusBadRequest)
			return
		}
		if payload.Model != "test-model" || len(payload.Tools) != 2 || payload.Tools[0].Function.Name != canvasToolName || payload.Tools[1].Function.Name != canvasOperationsTool {
			t.Errorf("unexpected request: %#v", payload)
		}
		if payload.ChatTemplateKwargs != nil {
			t.Errorf("thinking options were sent for a non-thinking model: %#v", payload.ChatTemplateKwargs)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello"}}]}`))
	}))
	defer server.Close()

	message, err := requestAI(server.Client(), server.URL, AIConfig{Model: "test-model", APIKey: "secret"}, []openAIMessage{{Role: "user", Content: "hi"}})
	if err != nil || message.Content != "hello" {
		t.Fatalf("message = %#v, error = %v", message, err)
	}
}

func TestOpenAIToolFreeFinalizationOmitsTools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(writer, "invalid request", http.StatusBadRequest)
			return
		}
		if _, present := payload["tools"]; present {
			t.Errorf("tool-free final request unexpectedly contains tools: %s", payload["tools"])
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"finished"}}]}`))
	}))
	defer server.Close()

	message, err := requestAIWithTools(server.Client(), server.URL, AIConfig{Model: "test-model"}, []openAIMessage{{Role: "user", Content: "summarize"}}, nil)
	if err != nil || message.Content != "finished" {
		t.Fatalf("message = %#v, error = %v", message, err)
	}
}

func TestOpenAIForcedToolChoice(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Tools      []any `json:"tools"`
			ToolChoice struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tool_choice"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if len(payload.Tools) != 2 || payload.ToolChoice.Type != "function" || payload.ToolChoice.Function.Name != canvasOperationsTool {
			t.Errorf("unexpected forced tool request: %#v", payload)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"apply_canvas_operations","arguments":"{}"}}]}}]}`))
	}))
	defer server.Close()

	message, err := requestAIWithToolChoice(
		server.Client(), server.URL, AIConfig{Model: "test-model"},
		[]openAIMessage{{Role: "user", Content: "draw a box"}},
		[]any{canvasTool, canvasOperationsToolDefinition}, forceToolChoice(canvasOperationsTool),
	)
	if err != nil || len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Name != canvasOperationsTool {
		t.Fatalf("message = %#v, error = %v", message, err)
	}
}

func TestOpenAIStreamingToolCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Stream             bool            `json:"stream"`
			ChatTemplateKwargs map[string]bool `json:"chat_template_kwargs"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if !payload.Stream {
			t.Error("stream was not enabled")
		}
		if !payload.ChatTemplateKwargs["enable_thinking"] {
			t.Error("thinking was not enabled")
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte(`data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"apply_canvas_","arguments":"{\"expected\""}}]}}]}` + "\n\n"))
		_, _ = writer.Write([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"operations","arguments":":true}"}}]}}]}` + "\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	message, err := requestAIWithToolChoice(
		server.Client(), server.URL, AIConfig{Model: "test-model", EnableThinking: true},
		[]openAIMessage{{Role: "user", Content: "draw a box"}},
		[]any{canvasTool, canvasOperationsToolDefinition}, forceToolChoice(canvasOperationsTool),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v", message.ToolCalls)
	}
	call := message.ToolCalls[0]
	if call.ID != "call-1" || call.Type != "function" || call.Function.Name != canvasOperationsTool || call.Function.Arguments != `{"expected":true}` {
		t.Fatalf("tool call = %#v", call)
	}
}

func TestOpenAIStreamingContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"Hello \"}}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"world\"}}]}\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	var streamed strings.Builder
	message, err := requestAIWithToolChoiceStream(
		server.Client(), server.URL, AIConfig{Model: "test-model"},
		[]openAIMessage{{Role: "user", Content: "hi"}},
		[]any{canvasTool, canvasOperationsToolDefinition}, nil,
		func(kind aiStreamKind, delta string) {
			if kind == aiStreamAnswer {
				streamed.WriteString(delta)
			}
		},
	)
	if err != nil || message.Content != "Hello world" {
		t.Fatalf("message = %#v, error = %v", message, err)
	}
	if streamed.String() != "Hello world" {
		t.Fatalf("streamed content = %q", streamed.String())
	}
}

func TestOpenAIStreamingThinking(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"Check \"}}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"thinking\":\"canvas. \",\"content\":\"Done\"}}]}\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	var thinking, answer strings.Builder
	message, err := requestAIWithToolChoiceStream(
		server.Client(), server.URL, AIConfig{Model: "test-model"},
		[]openAIMessage{{Role: "user", Content: "hi"}}, nil, nil,
		func(kind aiStreamKind, delta string) {
			if kind == aiStreamThinking {
				thinking.WriteString(delta)
			} else {
				answer.WriteString(delta)
			}
		},
	)
	if err != nil || message.Content != "Done" {
		t.Fatalf("message = %#v, error = %v", message, err)
	}
	if thinking.String() != "Check canvas. " || answer.String() != "Done" {
		t.Fatalf("thinking = %q, answer = %q", thinking.String(), answer.String())
	}
}

func TestOpenAIStreamingContentThoughtChannel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"th\"}}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ought\\nCheck \"}}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"the canvas\"}}]}\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	var thinking, answer strings.Builder
	message, err := requestAIWithToolChoiceStream(
		server.Client(), server.URL, AIConfig{Model: "test-model"},
		[]openAIMessage{{Role: "user", Content: "hi"}}, nil, nil,
		func(kind aiStreamKind, delta string) {
			if kind == aiStreamThinking {
				thinking.WriteString(delta)
			} else {
				answer.WriteString(delta)
			}
		},
	)
	if err != nil || message.Content != "thought\nCheck the canvas" {
		t.Fatalf("message = %#v, error = %v", message, err)
	}
	if thinking.String() != "thought\nCheck the canvas" || answer.Len() != 0 {
		t.Fatalf("thinking = %q, answer = %q", thinking.String(), answer.String())
	}
}

func TestSlidingResponseTimeout(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	body := newSlidingTimeoutBody(reader, 25*time.Millisecond)
	defer body.Close()

	_, err := body.Read(make([]byte, 1))
	if !errors.Is(err, errAIResponseIdleTimeout) {
		t.Fatalf("timeout error = %v", err)
	}
}

func TestCanvasEditIntent(t *testing.T) {
	for _, prompt := range []string{
		"Create a kanban board with three cards",
		"Please move the rectangle to the right",
		"delete the selected-looking element",
		"make a flowchart for the deployment",
	} {
		if !canvasEditIntent(prompt) {
			t.Errorf("expected edit intent for %q", prompt)
		}
	}
	for _, prompt := range []string{
		"What is on the canvas?",
		"Summarize this drawing",
		"How do I create a rectangle myself?",
	} {
		if canvasEditIntent(prompt) {
			t.Errorf("unexpected edit intent for %q", prompt)
		}
	}
}

func TestApplyCanvasOperations(t *testing.T) {
	document := []byte(`{"type":"excalidraw","version":2,"elements":[{"id":"keep","type":"rectangle","x":1,"y":2,"width":10,"height":20,"version":1,"versionNonce":1,"isDeleted":false}],"appState":{"viewBackgroundColor":"#fff"},"files":{}}`)
	arguments := []byte(`{"expectedFilename":"Board.excalidraw","operations":[{"op":"update","id":"keep","changes":{"x":25,"strokeColor":"#ff0000"}},{"op":"add","element":{"id":"new-text","type":"text","x":50,"y":60,"text":"Hello","originalText":"Hello"}},{"op":"delete","id":"keep"}]}`)

	updated, result, err := applyCanvasOperations(document, arguments, "Board.excalidraw")
	if err != nil {
		t.Fatal(err)
	}
	if result["ok"] != true {
		t.Fatalf("result = %#v", result)
	}
	var scene struct {
		Elements []map[string]any `json:"elements"`
		AppState map[string]any   `json:"appState"`
	}
	if err := json.Unmarshal(updated, &scene); err != nil {
		t.Fatal(err)
	}
	if len(scene.Elements) != 2 || scene.Elements[0]["x"] != float64(25) || scene.Elements[0]["isDeleted"] != true {
		t.Fatalf("updated existing element = %#v", scene.Elements)
	}
	if scene.Elements[0]["version"] != float64(3) {
		t.Fatalf("version = %#v, want 3", scene.Elements[0]["version"])
	}
	if scene.Elements[1]["id"] != "new-text" || scene.Elements[1]["type"] != "text" || scene.Elements[1]["width"] != float64(100) {
		t.Fatalf("new element = %#v", scene.Elements[1])
	}
	if scene.AppState["viewBackgroundColor"] != "#fff" {
		t.Fatalf("app state was not preserved: %#v", scene.AppState)
	}
}

func TestApplyCanvasOperationsRejectsUnsafeChanges(t *testing.T) {
	document := []byte(`{"elements":[{"id":"a","type":"rectangle","x":0,"y":0}]}`)
	cases := []string{
		`{"expectedFilename":"Board.excalidraw","operations":[{"op":"update","id":"a","changes":{"id":"b"}}]}`,
		`{"expectedFilename":"Board.excalidraw","operations":[{"op":"add","element":{"type":"rectangle","x":"bad","y":0}}]}`,
		`{"expectedFilename":"Board.excalidraw","operations":[{"op":"delete","id":"missing"}]}`,
		`{"expectedFilename":"Board.excalidraw","operations":[]}`,
		`{"expectedFilename":"Other.excalidraw","operations":[{"op":"delete","id":"a"}]}`,
		`{"expectedFilename":"Board.excalidraw","operations":[{"op":"update","id":"a","changes":{"madeUpField":true}}]}`,
		`{"expectedFilename":"Board.excalidraw","operations":[{"op":"add","element":{"type":"selection","x":0,"y":0}}]}`,
		`{"expectedFilename":"Board.excalidraw","operations":[{"op":"add","element":{"type":"rectangle","x":0,"y":0,"isDeleted":true}}]}`,
	}
	for _, arguments := range cases {
		if _, _, err := applyCanvasOperations(document, []byte(arguments), "Board.excalidraw"); err == nil {
			t.Errorf("expected %s to be rejected", arguments)
		}
	}
}

func TestCanvasToolCoversExcalidrawElementAPI(t *testing.T) {
	properties := canvasElementProperties(true)
	for _, field := range []string{
		"id", "type", "x", "y", "width", "height", "angle", "strokeColor", "backgroundColor",
		"fillStyle", "strokeWidth", "strokeStyle", "roughness", "opacity", "roundness", "groupIds",
		"frameId", "boundElements", "link", "locked", "customData", "points", "startBinding", "endBinding",
		"startArrowhead", "endArrowhead", "elbowed", "fixedSegments", "startIsSpecial", "endIsSpecial",
		"pressures", "simulatePressure", "text", "originalText", "fontSize", "fontFamily", "textAlign",
		"verticalAlign", "containerId", "autoResize", "lineHeight", "fileId", "status", "scale", "crop", "name",
	} {
		if _, ok := properties[field]; !ok {
			t.Errorf("canvas tool schema is missing %q", field)
		}
	}
	wantTypes := map[string]bool{
		"rectangle": true, "diamond": true, "ellipse": true, "line": true, "arrow": true,
		"text": true, "image": true, "freedraw": true, "frame": true, "magicframe": true,
		"iframe": true, "embeddable": true,
	}
	for _, elementType := range canvasElementTypes {
		delete(wantTypes, elementType)
	}
	if len(wantTypes) != 0 {
		t.Fatalf("canvas tool schema is missing element types: %#v", wantTypes)
	}
	if _, err := json.Marshal(canvasOperationsToolDefinition); err != nil {
		t.Fatalf("canvas tool definition is not JSON serializable: %v", err)
	}
}

func TestApplyCanvasOperationsSupportsZOrderAndTypeDefaults(t *testing.T) {
	document := []byte(`{"type":"excalidraw","version":2,"elements":[{"id":"back","type":"rectangle","x":0,"y":0},{"id":"front","type":"ellipse","x":10,"y":10}],"appState":{},"files":{}}`)
	arguments := []byte(`{"expectedFilename":"Board.excalidraw","operations":[{"op":"add","position":"after","referenceId":"back","element":{"id":"label","type":"text","x":5,"y":5,"text":"Hello"}},{"op":"add","position":"back","element":{"id":"connector","type":"arrow","x":0,"y":0}},{"op":"reorder","id":"front","position":"before","referenceId":"label"}]}`)

	updated, _, err := applyCanvasOperations(document, arguments, "Board.excalidraw")
	if err != nil {
		t.Fatal(err)
	}
	var scene struct {
		Elements []map[string]any `json:"elements"`
	}
	if err := json.Unmarshal(updated, &scene); err != nil {
		t.Fatal(err)
	}
	gotOrder := make([]string, len(scene.Elements))
	for index, element := range scene.Elements {
		gotOrder[index], _ = element["id"].(string)
	}
	wantOrder := []string{"connector", "back", "front", "label"}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Fatalf("z-order = %v, want %v", gotOrder, wantOrder)
	}
	connector := scene.Elements[0]
	if connector["endArrowhead"] != "arrow" || len(connector["points"].([]any)) != 2 {
		t.Fatalf("arrow defaults = %#v", connector)
	}
	label := scene.Elements[3]
	if label["originalText"] != "Hello" || label["fontSize"] != float64(20) {
		t.Fatalf("text defaults = %#v", label)
	}
}

func TestRequestAIReportsPlainTextServiceUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = writer.Write([]byte("no clients with model deepseek-v4-flash"))
	}))
	defer server.Close()

	_, err := requestAI(server.Client(), server.URL, AIConfig{Model: "deepseek-v4-flash"}, []openAIMessage{{Role: "user", Content: "hi"}})
	if err == nil || !strings.Contains(err.Error(), "no clients with model deepseek-v4-flash") || strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestApplicationZoomDefaultsAndValidation(t *testing.T) {
	store := testStore(t)
	if store.prefs.ApplicationZoom != defaultApplicationZoom {
		t.Fatalf("application zoom = %v", store.prefs.ApplicationZoom)
	}
	for _, zoom := range []float64{minApplicationZoom, 1, maxApplicationZoom} {
		if !validApplicationZoom(zoom) {
			t.Errorf("zoom %v should be valid", zoom)
		}
	}
	for _, zoom := range []float64{0, minApplicationZoom - 0.01, maxApplicationZoom + 0.01} {
		if validApplicationZoom(zoom) {
			t.Errorf("zoom %v should be invalid", zoom)
		}
	}
}
