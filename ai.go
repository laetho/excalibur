package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	canvasToolName         = "get_canvas_file"
	canvasOperationsTool   = "apply_canvas_operations"
	maxCanvasOperations    = 100
	maxCanvasOperationArgs = 2 << 20
	maxAIToolRounds        = 10
	aiResponseIdleTimeout  = 5 * time.Minute
)

type aiStreamKind string

const (
	aiStreamAnswer   aiStreamKind = "answer"
	aiStreamThinking aiStreamKind = "thinking"
)

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAIRequest struct {
	Model              string          `json:"model"`
	Messages           []openAIMessage `json:"messages"`
	Tools              []any           `json:"tools,omitempty"`
	ToolChoice         any             `json:"tool_choice,omitempty"`
	Stream             bool            `json:"stream"`
	ChatTemplateKwargs map[string]bool `json:"chat_template_kwargs,omitempty"`
}

type openAIResponse struct {
	Choices []struct {
		Message openAIMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type openAIStreamChunk struct {
	Choices []struct {
		Delta struct {
			Role             string `json:"role,omitempty"`
			Content          string `json:"content,omitempty"`
			ReasoningContent string `json:"reasoning_content,omitempty"`
			Reasoning        string `json:"reasoning,omitempty"`
			Thinking         string `json:"thinking,omitempty"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id,omitempty"`
				Type     string `json:"type,omitempty"`
				Function struct {
					Name      string `json:"name,omitempty"`
					Arguments string `json:"arguments,omitempty"`
				} `json:"function,omitempty"`
			} `json:"tool_calls,omitempty"`
		} `json:"delta"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type openAIModelsResponse struct {
	Data []struct {
		ID        string `json:"id"`
		Model     string `json:"model"`
		Reasoning bool   `json:"reasoning"`
		Options   struct {
			ChatTemplateKwargs map[string]bool `json:"chat_template_kwargs"`
		} `json:"options"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

var canvasTool = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name":        canvasToolName,
		"description": "Read the currently open Excalidraw canvas file. Returns its filename and complete .excalidraw JSON, including elements, app state, and embedded files. Call this when the user's request depends on the drawing.",
		"parameters": map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": false,
		},
	},
}

var canvasElementTypes = []string{
	"rectangle", "diamond", "ellipse", "line", "arrow", "text", "image",
	"freedraw", "frame", "magicframe", "iframe", "embeddable",
}

func numberPairSchema(description string) map[string]any {
	return map[string]any{
		"type": "array", "description": description,
		"items": map[string]any{"type": "number"}, "minItems": 2, "maxItems": 2,
	}
}

func pointBindingSchema() map[string]any {
	return map[string]any{
		"type": []string{"object", "null"},
		"properties": map[string]any{
			"elementId":  map[string]any{"type": "string"},
			"focus":      map[string]any{"type": "number"},
			"gap":        map[string]any{"type": "number"},
			"fixedPoint": numberPairSchema("Optional normalized binding point [x,y]."),
		},
		"required": []string{"elementId", "focus", "gap"}, "additionalProperties": false,
	}
}

// canvasElementProperties documents the public Excalidraw element model used by
// @excalidraw/excalidraw. Keeping this explicit matters: models otherwise tend
// to invent CSS-like fields or encode points as objects/strings.
func canvasElementProperties(includeProtected bool) map[string]any {
	properties := map[string]any{
		"x":               map[string]any{"type": "number", "description": "Absolute horizontal scene coordinate."},
		"y":               map[string]any{"type": "number", "description": "Absolute vertical scene coordinate."},
		"width":           map[string]any{"type": "number", "minimum": 0},
		"height":          map[string]any{"type": "number", "minimum": 0},
		"angle":           map[string]any{"type": "number", "description": "Rotation in radians."},
		"strokeColor":     map[string]any{"type": "string", "description": "CSS color such as #1e1e1e."},
		"backgroundColor": map[string]any{"type": "string", "description": "CSS color or transparent."},
		"fillStyle":       map[string]any{"type": "string", "enum": []string{"hachure", "cross-hatch", "solid", "zigzag"}},
		"strokeWidth":     map[string]any{"type": "number", "minimum": 0},
		"strokeStyle":     map[string]any{"type": "string", "enum": []string{"solid", "dashed", "dotted"}},
		"roughness":       map[string]any{"type": "number", "minimum": 0},
		"opacity":         map[string]any{"type": "number", "minimum": 0, "maximum": 100},
		"roundness": map[string]any{
			"type":       []string{"object", "null"},
			"properties": map[string]any{"type": map[string]any{"type": "integer"}, "value": map[string]any{"type": "number"}},
			"required":   []string{"type"}, "additionalProperties": false,
		},
		"groupIds": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Nested group IDs, deepest first."},
		"frameId":  map[string]any{"type": []string{"string", "null"}, "description": "ID of the containing frame."},
		"boundElements": map[string]any{
			"type": []string{"array", "null"}, "description": "Arrows and text bound to this element.",
			"items": map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}, "type": map[string]any{"type": "string", "enum": []string{"arrow", "text"}}}, "required": []string{"id", "type"}, "additionalProperties": false},
		},
		"link":       map[string]any{"type": []string{"string", "null"}},
		"locked":     map[string]any{"type": "boolean"},
		"customData": map[string]any{"type": "object", "description": "JSON-serializable application metadata."},

		// Linear and free-draw elements.
		"points": map[string]any{
			"type": "array", "description": "line/arrow/freedraw points relative to x/y. Close a polygon by repeating its first point.",
			"items": numberPairSchema("A relative [x,y] point."), "minItems": 2,
		},
		"lastCommittedPoint": map[string]any{"oneOf": []any{numberPairSchema("Last committed relative point."), map[string]any{"type": "null"}}},
		"startBinding":       pointBindingSchema(),
		"endBinding":         pointBindingSchema(),
		"startArrowhead":     map[string]any{"type": []string{"string", "null"}, "enum": []any{"arrow", "bar", "dot", "circle", "circle_outline", "triangle", "triangle_outline", "diamond", "diamond_outline", "crowfoot_one", "crowfoot_many", "crowfoot_one_or_many", nil}},
		"endArrowhead":       map[string]any{"type": []string{"string", "null"}, "enum": []any{"arrow", "bar", "dot", "circle", "circle_outline", "triangle", "triangle_outline", "diamond", "diamond_outline", "crowfoot_one", "crowfoot_many", "crowfoot_one_or_many", nil}},
		"elbowed":            map[string]any{"type": "boolean", "description": "Whether an arrow is an elbow arrow."},
		"fixedSegments":      map[string]any{"type": []string{"array", "null"}, "items": map[string]any{"type": "object", "properties": map[string]any{"start": numberPairSchema("Segment start."), "end": numberPairSchema("Segment end."), "index": map[string]any{"type": "integer"}}, "required": []string{"start", "end", "index"}, "additionalProperties": false}},
		"startIsSpecial":     map[string]any{"type": []string{"boolean", "null"}, "description": "Elbow-arrow special start segment marker."},
		"endIsSpecial":       map[string]any{"type": []string{"boolean", "null"}, "description": "Elbow-arrow special end segment marker."},
		"pressures":          map[string]any{"type": "array", "items": map[string]any{"type": "number", "minimum": 0, "maximum": 1}},
		"simulatePressure":   map[string]any{"type": "boolean"},

		// Text elements.
		"text":          map[string]any{"type": "string"},
		"originalText":  map[string]any{"type": "string"},
		"fontSize":      map[string]any{"type": "number", "exclusiveMinimum": 0},
		"fontFamily":    map[string]any{"type": "integer", "description": "Excalidraw font family ID."},
		"textAlign":     map[string]any{"type": "string", "enum": []string{"left", "center", "right"}},
		"verticalAlign": map[string]any{"type": "string", "enum": []string{"top", "middle", "bottom"}},
		"containerId":   map[string]any{"type": []string{"string", "null"}},
		"autoResize":    map[string]any{"type": "boolean"},
		"lineHeight":    map[string]any{"type": "number", "exclusiveMinimum": 0},

		// Images, frames, iframes, and embeddables.
		"fileId": map[string]any{"type": []string{"string", "null"}, "description": "Existing key in the document files map."},
		"status": map[string]any{"type": "string", "enum": []string{"pending", "saved", "error"}},
		"scale":  numberPairSchema("Image axis scale; each value is normally 1 or -1."),
		"crop":   map[string]any{"type": []string{"object", "null"}, "properties": map[string]any{"x": map[string]any{"type": "number"}, "y": map[string]any{"type": "number"}, "width": map[string]any{"type": "number"}, "height": map[string]any{"type": "number"}, "naturalWidth": map[string]any{"type": "number"}, "naturalHeight": map[string]any{"type": "number"}}, "required": []string{"x", "y", "width", "height", "naturalWidth", "naturalHeight"}, "additionalProperties": false},
		"name":   map[string]any{"type": []string{"string", "null"}, "description": "Frame or magic-frame name."},
	}
	if includeProtected {
		properties["id"] = map[string]any{"type": "string"}
		properties["type"] = map[string]any{"type": "string", "enum": canvasElementTypes}
	}
	return properties
}

var canvasOperationsToolDefinition = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name":        canvasOperationsTool,
		"description": "Modify the open Excalidraw scene with complete element CRUD and z-order operations. Call get_canvas_file first. Supports every public Excalidraw element type except transient selection elements, all common style/geometry/group/frame/link fields, and type-specific text, linear, arrow, freedraw, image, frame, iframe, and embeddable fields. Updates are undoable. Coordinates and point pairs must be JSON numbers. Excalidraw has no triangle type: use a closed line such as [[0,100],[50,0],[100,100],[0,100]].",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"expectedFilename": map[string]any{"type": "string", "description": "The exact filename returned by get_canvas_file. The edit is rejected if the user has switched canvases."},
				"operations": map[string]any{
					"type":     "array",
					"minItems": 1,
					"maxItems": maxCanvasOperations,
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"op":          map[string]any{"type": "string", "enum": []string{"add", "update", "delete", "reorder"}},
							"id":          map[string]any{"type": "string", "description": "Existing element ID for update/delete/reorder; optional preferred ID for add."},
							"position":    map[string]any{"type": "string", "enum": []string{"front", "back", "before", "after"}, "description": "Optional z-order placement for add; required for reorder."},
							"referenceId": map[string]any{"type": "string", "description": "Existing element used by before/after placement."},
							"element": map[string]any{
								"type":                 "object",
								"description":          "Element for add. Use ellipse for circles. For iframe/embeddable set link. Image fileId must already exist in the file returned by get_canvas_file.",
								"properties":           canvasElementProperties(true),
								"required":             []string{"type", "x", "y"},
								"additionalProperties": false,
							},
							"changes": map[string]any{"type": "object", "description": "Element fields to change. id, type, ordering index, version metadata, and deletion state are managed by Excalidraw and cannot be changed.", "properties": canvasElementProperties(false), "additionalProperties": false, "minProperties": 1},
						},
						"required":             []string{"op"},
						"additionalProperties": false,
					},
				},
			},
			"required":             []string{"expectedFilename", "operations"},
			"additionalProperties": false,
		},
	},
}

type canvasOperation struct {
	Op          string         `json:"op"`
	ID          string         `json:"id,omitempty"`
	Position    string         `json:"position,omitempty"`
	ReferenceID string         `json:"referenceId,omitempty"`
	Element     map[string]any `json:"element,omitempty"`
	Changes     map[string]any `json:"changes,omitempty"`
}

type canvasOperationsInput struct {
	ExpectedFilename string            `json:"expectedFilename"`
	Operations       []canvasOperation `json:"operations"`
}

func chatCompletionsURL(base string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", errors.New("enter a valid HTTP or HTTPS Dramallama URL")
	}
	path := strings.TrimRight(parsed.Path, "/")
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		// The user supplied the complete endpoint.
	case strings.HasSuffix(path, "/v1"):
		path += "/chat/completions"
	default:
		path += "/v1/chat/completions"
	}
	parsed.Path = path
	return parsed.String(), nil
}

// aiModelsURL deliberately uses the gateway's fixed discovery route rather
// than deriving it from the chat-completions path.
func aiModelsURL(base string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", errors.New("enter a valid HTTP or HTTPS Dramallama URL")
	}
	parsed.Path = "/gate/v1/models"
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func (a *App) effectiveAIKey(replacement string, clear bool) (string, error) {
	if clear {
		return "", nil
	}
	if replacement != "" {
		return replacement, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store == nil {
		return "", errors.New("store is not ready")
	}
	return a.store.prefs.AI.APIKey, nil
}

// DiscoverAIModels returns the model IDs advertised by Dramallama's gateway.
// A newly entered key takes precedence; otherwise the saved key is used.
func (a *App) DiscoverAIModels(baseURL, apiKey string, clearAPIKey bool) ([]AIModel, error) {
	endpoint, err := aiModelsURL(baseURL)
	if err != nil {
		return nil, err
	}
	key, err := a.effectiveAIKey(apiKey, clearAPIKey)
	if err != nil {
		return nil, err
	}
	return requestAIModels(&http.Client{Timeout: 15 * time.Second}, endpoint, key)
}

func (a *App) SetAISettings(update AISettingsUpdate) error {
	update.BaseURL = strings.TrimSpace(update.BaseURL)
	update.Model = strings.TrimSpace(update.Model)
	if update.Enabled {
		if _, err := chatCompletionsURL(update.BaseURL); err != nil {
			return err
		}
		if update.Model == "" {
			return errors.New("choose a model")
		}
		models, err := a.DiscoverAIModels(update.BaseURL, update.APIKey, update.ClearAPIKey)
		if err != nil {
			return fmt.Errorf("could not validate the model: %w", err)
		}
		if !modelAvailable(models, update.Model) {
			return fmt.Errorf("model %q is not available from Dramallama", update.Model)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store == nil {
		return errors.New("store is not ready")
	}
	config := &a.store.prefs.AI
	config.Enabled, config.BaseURL, config.Model = update.Enabled, update.BaseURL, update.Model
	if update.ClearAPIKey {
		config.APIKey = ""
	} else if update.APIKey != "" {
		config.APIKey = update.APIKey
	}
	a.store.savePrefs()
	a.emitSnapshotLocked()
	return nil
}

func modelAvailable(models []AIModel, selected string) bool {
	for _, model := range models {
		if model.Model == selected {
			return true
		}
	}
	return false
}

func requestAIModels(client *http.Client, endpoint, apiKey string) ([]AIModel, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("model discovery failed: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var result openAIModelsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("Dramallama returned invalid model JSON (HTTP %d)", response.StatusCode)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := strings.TrimSpace(string(body))
		if result.Error != nil && result.Error.Message != "" {
			message = result.Error.Message
		}
		if len(message) > 500 {
			message = message[:500]
		}
		return nil, fmt.Errorf("Dramallama returned HTTP %d: %s", response.StatusCode, message)
	}
	seen := make(map[string]struct{}, len(result.Data))
	models := make([]AIModel, 0, len(result.Data))
	for _, item := range result.Data {
		item.ID = strings.TrimSpace(item.ID)
		model := strings.TrimSpace(item.Model)
		if model == "" {
			continue
		}
		if _, exists := seen[model]; exists {
			continue
		}
		seen[model] = struct{}{}
		if item.ID == "" {
			item.ID = model
		}
		thinking := item.Reasoning
		if enabled, advertised := item.Options.ChatTemplateKwargs["enable_thinking"]; advertised {
			thinking = enabled
		}
		models = append(models, AIModel{ID: item.ID, Model: model, Thinking: thinking})
	}
	if len(models) == 0 {
		return nil, errors.New("Dramallama returned no models")
	}
	sort.Slice(models, func(i, j int) bool {
		return strings.ToLower(models[i].ID) < strings.ToLower(models[j].ID)
	})
	return models, nil
}

// AIChat runs a small OpenAI-compatible tool loop. Dramallama can inspect the
// current canvas and make bounded, granular, undoable element changes.
func (a *App) AIChat(history []AIChatMessage, model, requestID string, enableThinking bool) (string, error) {
	a.mu.Lock()
	if a.store == nil {
		a.mu.Unlock()
		return "", errors.New("store is not ready")
	}
	config := a.store.prefs.AI
	a.mu.Unlock()
	if !config.Enabled {
		return "", errors.New("Dramallama is disabled in Settings")
	}
	model = strings.TrimSpace(model)
	if model == "" {
		model = config.Model
	}
	if model == "" {
		return "", errors.New("choose a model")
	}
	config.Model = model
	config.EnableThinking = enableThinking
	endpoint, err := chatCompletionsURL(config.BaseURL)
	if err != nil {
		return "", err
	}
	if len(history) == 0 || len(history) > 100 {
		return "", errors.New("invalid chat history")
	}

	messages := []openAIMessage{{
		Role:    "system",
		Content: "You are the AI assistant inside Excalibur. Use get_canvas_file whenever answering requires seeing the current drawing. You can modify the drawing with apply_canvas_operations, which exposes the full persisted Excalidraw element model plus z-ordering. Always inspect first, preserve unrelated elements, use existing IDs for updates/deletes/reordering, and prefer a small number of precise operations. x/y are absolute scene coordinates; line, arrow, and freedraw points are numeric [x,y] pairs relative to the element's x/y. Use ellipse for circles. Excalidraw has no triangle type, so use a closed line. Use frameId to place elements in frames, groupIds for groups, containerId/boundElements for bound text, and startBinding/endBinding for connected arrows. Image fileId values must already exist in the inspected document's files map. Use reorder instead of trying to edit Excalidraw's managed index field. After an edit succeeds, normally report the result instead of repeatedly reading and editing. When a tool reports an error, correct the arguments and retry. Never claim to have inspected or changed the canvas unless the corresponding tool succeeded.",
	}}
	for _, message := range history {
		if (message.Role != "user" && message.Role != "assistant") || strings.TrimSpace(message.Content) == "" {
			return "", errors.New("invalid chat message")
		}
		messages = append(messages, openAIMessage{Role: message.Role, Content: message.Content})
	}

	client := newAIHTTPClient()
	editRequested := false
	for index := len(history) - 1; index >= 0; index-- {
		if history[index].Role == "user" {
			editRequested = canvasEditIntent(history[index].Content)
			break
		}
	}
	canvasInspected := false
	canvasEdited := false
	emitDelta := func(kind aiStreamKind, delta string) {
		if delta != "" && requestID != "" {
			a.emit("ai:stream", map[string]string{"requestId": requestID, "kind": string(kind), "delta": delta})
		}
	}
	for turn := 0; turn < maxAIToolRounds; turn++ {
		var toolChoice any
		if editRequested && !canvasInspected {
			toolChoice = forceToolChoice(canvasToolName)
		} else if editRequested && !canvasEdited {
			toolChoice = forceToolChoice(canvasOperationsTool)
		}
		answerIsSafe := !editRequested || canvasEdited
		onDelta := func(kind aiStreamKind, delta string) {
			// Models may reason while preparing required tool calls. Surface that
			// explicitly, but continue hiding premature prose that could falsely
			// imply a requested canvas edit already succeeded.
			if kind == aiStreamThinking || answerIsSafe {
				emitDelta(kind, delta)
			}
		}
		message, err := requestAIWithToolChoiceStream(client, endpoint, config, messages, []any{canvasTool, canvasOperationsToolDefinition}, toolChoice, onDelta)
		if err != nil {
			return "", err
		}
		if len(message.ToolCalls) == 0 {
			content, ok := message.Content.(string)
			if !ok || strings.TrimSpace(content) == "" {
				return "", errors.New("Dramallama returned an empty response")
			}
			if editRequested && !canvasEdited {
				// Some OpenAI-compatible models ignore tool_choice and print a JSON
				// blueprint as prose. Do not present that as a successful canvas edit.
				messages = append(messages, message, openAIMessage{
					Role:    "system",
					Content: "The user requested an actual canvas edit, but you answered with prose. Invoke the required function tool now. Do not print an element blueprint in the chat.",
				})
				continue
			}
			return content, nil
		}
		messages = append(messages, message)
		for _, call := range message.ToolCalls {
			result := ""
			switch call.Function.Name {
			case canvasToolName:
				result, err = a.canvasFileForAI()
				if err != nil {
					encoded, _ := json.Marshal(map[string]string{"error": err.Error()})
					result = string(encoded)
				} else {
					canvasInspected = true
				}
			case canvasOperationsTool:
				result, err = a.applyCanvasOperationsForAI(call.Function.Arguments)
				if err != nil {
					encoded, _ := json.Marshal(map[string]string{"error": err.Error()})
					result = string(encoded)
				} else {
					canvasEdited = true
				}
			default:
				result = fmt.Sprintf(`{"error":"unsupported tool %q"}`, call.Function.Name)
			}
			messages = append(messages, openAIMessage{Role: "tool", ToolCallID: call.ID, Content: result})
		}
	}
	if editRequested && !canvasEdited {
		return "", errors.New("Dramallama did not invoke the required canvas editing tool; no canvas changes were applied")
	}

	// Do not fail the whole chat after useful canvas work just because the model
	// kept iterating. Give it one tool-free turn to summarize what it completed.
	messages = append(messages, openAIMessage{
		Role:    "system",
		Content: fmt.Sprintf("The canvas tool budget of %d rounds is exhausted. Do not request more tools. Briefly summarize the work completed so far and any remaining work.", maxAIToolRounds),
	})
	message, err := requestAIWithToolChoiceStream(client, endpoint, config, messages, nil, nil, emitDelta)
	if err != nil {
		return "", err
	}
	content, ok := message.Content.(string)
	if !ok || strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("Dramallama did not finish after %d iterative canvas tool rounds", maxAIToolRounds)
	}
	return content, nil
}

func requestAI(client *http.Client, endpoint string, config AIConfig, messages []openAIMessage) (openAIMessage, error) {
	return requestAIWithTools(client, endpoint, config, messages, []any{canvasTool, canvasOperationsToolDefinition})
}

func requestAIWithTools(client *http.Client, endpoint string, config AIConfig, messages []openAIMessage, tools []any) (openAIMessage, error) {
	return requestAIWithToolChoice(client, endpoint, config, messages, tools, nil)
}

func requestAIWithToolChoice(client *http.Client, endpoint string, config AIConfig, messages []openAIMessage, tools []any, toolChoice any) (openAIMessage, error) {
	return requestAIWithToolChoiceStream(client, endpoint, config, messages, tools, toolChoice, nil)
}

func requestAIWithToolChoiceStream(client *http.Client, endpoint string, config AIConfig, messages []openAIMessage, tools []any, toolChoice any, onDelta func(aiStreamKind, string)) (openAIMessage, error) {
	var chatTemplateKwargs map[string]bool
	if config.EnableThinking {
		chatTemplateKwargs = map[string]bool{"enable_thinking": true}
	}
	payload, err := json.Marshal(openAIRequest{
		Model: config.Model, Messages: messages, Tools: tools, ToolChoice: toolChoice, Stream: true,
		ChatTemplateKwargs: chatTemplateKwargs,
	})
	if err != nil {
		return openAIMessage{}, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return openAIMessage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+config.APIKey)
	}
	response, err := client.Do(req)
	if err != nil {
		return openAIMessage{}, fmt.Errorf("Dramallama request failed: %w", err)
	}
	response.Body = newSlidingTimeoutBody(response.Body, aiResponseIdleTimeout)
	defer response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode < 300 && strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		return decodeAIStream(response.Body, onDelta)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return openAIMessage{}, err
	}
	var result openAIResponse
	decodeErr := json.Unmarshal(body, &result)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := boundedResponseMessage(body)
		if decodeErr == nil && result.Error != nil && result.Error.Message != "" {
			message = result.Error.Message
		}
		if message == "" {
			message = http.StatusText(response.StatusCode)
		}
		if response.StatusCode == http.StatusServiceUnavailable {
			return openAIMessage{}, fmt.Errorf("Dramallama has no available client for this request (HTTP 503): %s", message)
		}
		return openAIMessage{}, fmt.Errorf("Dramallama returned HTTP %d: %s", response.StatusCode, message)
	}
	if decodeErr != nil {
		message := boundedResponseMessage(body)
		if message == "" {
			return openAIMessage{}, fmt.Errorf("Dramallama returned invalid JSON (HTTP %d)", response.StatusCode)
		}
		return openAIMessage{}, fmt.Errorf("Dramallama returned invalid JSON (HTTP %d): %s", response.StatusCode, message)
	}
	if len(result.Choices) == 0 {
		return openAIMessage{}, errors.New("Dramallama returned no choices")
	}
	return result.Choices[0].Message, nil
}

func decodeAIStream(reader io.Reader, onDelta func(aiStreamKind, string)) (openAIMessage, error) {
	message := openAIMessage{Role: "assistant"}
	var content strings.Builder
	toolCalls := make(map[int]*openAIToolCall)
	maxToolIndex := -1
	seenEvent := false
	contentModeKnown := false
	contentIsThinking := false
	var pendingContent strings.Builder
	emitContent := func(delta string) {
		if onDelta == nil || delta == "" {
			return
		}
		kind := aiStreamAnswer
		if contentIsThinking {
			kind = aiStreamThinking
		}
		onDelta(kind, delta)
	}

	scanner := bufio.NewScanner(io.LimitReader(reader, 16<<20))
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		seenEvent = true
		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return openAIMessage{}, fmt.Errorf("Dramallama returned an invalid streaming event: %w", err)
		}
		if chunk.Error != nil && chunk.Error.Message != "" {
			return openAIMessage{}, fmt.Errorf("Dramallama stream failed: %s", chunk.Error.Message)
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Role != "" {
				message.Role = choice.Delta.Role
			}
			thinking := choice.Delta.ReasoningContent + choice.Delta.Reasoning + choice.Delta.Thinking
			if onDelta != nil && thinking != "" {
				onDelta(aiStreamThinking, thinking)
			}
			contentDelta := choice.Delta.Content
			content.WriteString(contentDelta)
			if contentDelta != "" {
				if !contentModeKnown {
					pendingContent.WriteString(contentDelta)
					pending := pendingContent.String()
					trimmed := strings.ToLower(strings.TrimLeft(pending, " \t\r\n"))
					// Some thinking-capable chat templates expose their channel as a
					// leading content marker instead of reasoning_content. Keep that
					// stream collapsible even when the inference server has no
					// reasoning parser configured.
					for _, marker := range []string{"thought\n", "analysis\n"} {
						if strings.HasPrefix(trimmed, marker) {
							contentIsThinking = true
							contentModeKnown = true
							break
						}
					}
					if !contentModeKnown && trimmed != "" {
						couldBeMarker := false
						for _, marker := range []string{"thought\n", "analysis\n"} {
							if strings.HasPrefix(marker, trimmed) {
								couldBeMarker = true
								break
							}
						}
						contentModeKnown = !couldBeMarker
					}
					if contentModeKnown {
						emitContent(pending)
						pendingContent.Reset()
					}
				} else {
					emitContent(contentDelta)
				}
			}
			for _, delta := range choice.Delta.ToolCalls {
				call := toolCalls[delta.Index]
				if call == nil {
					call = &openAIToolCall{}
					toolCalls[delta.Index] = call
				}
				if delta.Index > maxToolIndex {
					maxToolIndex = delta.Index
				}
				call.ID += delta.ID
				call.Type += delta.Type
				call.Function.Name += delta.Function.Name
				call.Function.Arguments += delta.Function.Arguments
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return openAIMessage{}, fmt.Errorf("could not read Dramallama stream: %w", err)
	}
	if pendingContent.Len() > 0 {
		contentIsThinking = false
		emitContent(pendingContent.String())
	}
	if !seenEvent {
		return openAIMessage{}, errors.New("Dramallama returned an empty stream")
	}
	if content.Len() > 0 {
		message.Content = content.String()
	}
	if maxToolIndex >= 0 {
		message.ToolCalls = make([]openAIToolCall, 0, len(toolCalls))
		for index := 0; index <= maxToolIndex; index++ {
			if call := toolCalls[index]; call != nil {
				message.ToolCalls = append(message.ToolCalls, *call)
			}
		}
	}
	if message.Content == nil && len(message.ToolCalls) == 0 {
		return openAIMessage{}, errors.New("Dramallama returned an empty streamed response")
	}
	return message, nil
}

var errAIResponseIdleTimeout = errors.New("Dramallama response timed out")

// newAIHTTPClient limits the wait for response headers, then lets an active SSE
// response continue indefinitely. The body wrapper below resets its five-minute
// idle window whenever any response bytes arrive.
func newAIHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = aiResponseIdleTimeout
	return &http.Client{Transport: transport}
}

type slidingTimeoutBody struct {
	body     io.ReadCloser
	timeout  time.Duration
	activity chan struct{}
	done     chan struct{}
	doneOnce sync.Once
	timedOut atomic.Bool
}

func newSlidingTimeoutBody(body io.ReadCloser, timeout time.Duration) io.ReadCloser {
	wrapped := &slidingTimeoutBody{
		body: body, timeout: timeout,
		activity: make(chan struct{}, 1), done: make(chan struct{}),
	}
	go wrapped.watch()
	return wrapped
}

func (body *slidingTimeoutBody) watch() {
	timer := time.NewTimer(body.timeout)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			body.timedOut.Store(true)
			_ = body.body.Close()
			return
		case <-body.activity:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(body.timeout)
		case <-body.done:
			return
		}
	}
}

func (body *slidingTimeoutBody) Read(buffer []byte) (int, error) {
	count, err := body.body.Read(buffer)
	if count > 0 {
		select {
		case body.activity <- struct{}{}:
		default:
		}
	}
	if err != nil && body.timedOut.Load() {
		return count, fmt.Errorf("%w after %s without data", errAIResponseIdleTimeout, body.timeout)
	}
	return count, err
}

func (body *slidingTimeoutBody) Close() error {
	body.doneOnce.Do(func() { close(body.done) })
	return body.body.Close()
}

func forceToolChoice(name string) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": name,
		},
	}
}

// canvasEditIntent is deliberately conservative. It only forces the tool
// state machine for direct imperative edit language; ordinary questions keep
// normal automatic tool selection.
func canvasEditIntent(prompt string) bool {
	lower := strings.ToLower(strings.TrimSpace(prompt))
	for _, prefix := range []string{"how ", "what ", "why ", "when ", "where ", "who ", "can you explain", "tell me how"} {
		if strings.HasPrefix(lower, prefix) {
			return false
		}
	}
	for _, phrase := range []string{
		"add ", "create ", "draw ", "insert ", "update ", "change ", "edit ",
		"modify ", "delete ", "remove ", "move ", "resize ", "recolor ",
		"colour ", "color ", "replace ", "connect ", "arrange ", "build ", "design ", "generate ",
		"put ", "write on ", "make the canvas", "make this canvas", "make a diagram",
		"make a flowchart", "make a board", "make a card",
	} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

func boundedResponseMessage(body []byte) string {
	message := strings.TrimSpace(string(body))
	message = strings.Join(strings.Fields(message), " ")
	if len(message) > 500 {
		message = message[:500]
	}
	return message
}

func (a *App) canvasFileForAI() (string, error) {
	name, data, err := a.canvasDataForAI()
	if err != nil {
		return "", err
	}
	result := map[string]any{"filename": name, "mimeType": "application/vnd.excalidraw+json"}
	if json.Valid(data) {
		result["content"] = json.RawMessage(data)
	} else {
		result["content"] = string(data)
	}
	encoded, err := json.Marshal(result)
	return string(encoded), err
}

// canvasDataForAI obtains the freshest webview state. takePending consumes the
// frontend's pending save, so persist it immediately before returning it.
func (a *App) canvasDataForAI() (string, []byte, error) {
	if pending := a.takePending(); pending != nil {
		if err := a.CanvasChange(pending.Name, pending.JSON); err != nil {
			return "", nil, err
		}
		return pending.Name + fileExtension, []byte(pending.JSON), nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store == nil || a.store.selection == "" {
		return "", nil, errors.New("no canvas is currently selected")
	}
	board := a.store.board(a.store.selection)
	if board == nil {
		return "", nil, errors.New("the selected canvas is unavailable")
	}
	data, err := a.store.readBoard(*board)
	return board.ID, data, err
}

func (a *App) applyCanvasOperationsForAI(arguments string) (string, error) {
	if len(arguments) > maxCanvasOperationArgs {
		return "", errors.New("canvas operation request is too large")
	}
	name, data, err := a.canvasDataForAI()
	if err != nil {
		return "", err
	}
	updated, result, err := applyCanvasOperations(data, []byte(arguments), name)
	if err != nil {
		return "", err
	}
	// The frontend restores/normalizes the elements before applying them and
	// captures the update in Excalidraw's undo stack. Its regular onChange path
	// then persists and broadcasts the scene.
	a.emit("canvas:ai-operations", map[string]any{"json": string(updated)})
	result["filename"] = name
	encoded, err := json.Marshal(result)
	return string(encoded), err
}

func applyCanvasOperations(document, arguments []byte, currentFilename string) ([]byte, map[string]any, error) {
	if !json.Valid(document) {
		return nil, nil, errors.New("the current canvas is not valid JSON")
	}
	var input canvasOperationsInput
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.UseNumber()
	if err := decoder.Decode(&input); err != nil {
		return nil, nil, fmt.Errorf("invalid canvas operations: %w", err)
	}
	if strings.TrimSpace(input.ExpectedFilename) == "" || input.ExpectedFilename != currentFilename {
		return nil, nil, errors.New("the open canvas changed; call get_canvas_file again before editing")
	}
	if len(input.Operations) == 0 || len(input.Operations) > maxCanvasOperations {
		return nil, nil, fmt.Errorf("provide between 1 and %d canvas operations", maxCanvasOperations)
	}
	var scene map[string]any
	documentDecoder := json.NewDecoder(bytes.NewReader(document))
	documentDecoder.UseNumber()
	if err := documentDecoder.Decode(&scene); err != nil {
		return nil, nil, fmt.Errorf("could not read the canvas: %w", err)
	}
	rawElements, ok := scene["elements"].([]any)
	if !ok {
		return nil, nil, errors.New("the canvas has no elements array")
	}
	elements := make([]map[string]any, len(rawElements))
	byID := make(map[string]int, len(rawElements))
	for i, raw := range rawElements {
		element, ok := raw.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("canvas element %d is invalid", i)
		}
		elements[i] = element
		if id, _ := element["id"].(string); id != "" {
			byID[id] = i
		}
	}

	applied := make([]map[string]string, 0, len(input.Operations))
	for index, operation := range input.Operations {
		op := strings.ToLower(strings.TrimSpace(operation.Op))
		id := strings.TrimSpace(operation.ID)
		switch op {
		case "add":
			if operation.Element == nil {
				return nil, nil, fmt.Errorf("operation %d: add requires element", index+1)
			}
			element := operation.Element
			if id == "" {
				id, _ = element["id"].(string)
				id = strings.TrimSpace(id)
			}
			if id == "" {
				id = fmt.Sprintf("ai-%d-%d", time.Now().UnixNano(), index)
			}
			if _, exists := byID[id]; exists {
				return nil, nil, fmt.Errorf("operation %d: element %q already exists", index+1, id)
			}
			if err := validateNewCanvasElement(element); err != nil {
				return nil, nil, fmt.Errorf("operation %d: %w", index+1, err)
			}
			element["id"] = id
			setElementDefaults(element, index)
			elements = append(elements, element)
			if err := placeCanvasElement(&elements, id, operation.Position, operation.ReferenceID); err != nil {
				return nil, nil, fmt.Errorf("operation %d: %w", index+1, err)
			}
			byID = canvasElementPositions(elements)
		case "update":
			position, exists := byID[id]
			if id == "" || !exists {
				return nil, nil, fmt.Errorf("operation %d: element %q was not found", index+1, id)
			}
			if len(operation.Changes) == 0 {
				return nil, nil, fmt.Errorf("operation %d: update requires changes", index+1)
			}
			for key, value := range operation.Changes {
				if forbiddenCanvasChange(key) {
					return nil, nil, fmt.Errorf("operation %d: field %q cannot be changed", index+1, key)
				}
				if _, supported := canvasElementProperties(false)[key]; !supported {
					return nil, nil, fmt.Errorf("operation %d: unsupported element field %q", index+1, key)
				}
				if err := validateCanvasChange(key, value); err != nil {
					return nil, nil, fmt.Errorf("operation %d: %w", index+1, err)
				}
				elements[position][key] = value
			}
			bumpElementVersion(elements[position])
		case "delete":
			position, exists := byID[id]
			if id == "" || !exists {
				return nil, nil, fmt.Errorf("operation %d: element %q was not found", index+1, id)
			}
			elements[position]["isDeleted"] = true
			bumpElementVersion(elements[position])
		case "reorder":
			if id == "" {
				return nil, nil, fmt.Errorf("operation %d: reorder requires id", index+1)
			}
			if _, exists := byID[id]; !exists {
				return nil, nil, fmt.Errorf("operation %d: element %q was not found", index+1, id)
			}
			if strings.TrimSpace(operation.Position) == "" {
				return nil, nil, fmt.Errorf("operation %d: reorder requires position", index+1)
			}
			if err := placeCanvasElement(&elements, id, operation.Position, operation.ReferenceID); err != nil {
				return nil, nil, fmt.Errorf("operation %d: %w", index+1, err)
			}
			byID = canvasElementPositions(elements)
		default:
			return nil, nil, fmt.Errorf("operation %d: unsupported operation %q", index+1, operation.Op)
		}
		applied = append(applied, map[string]string{"op": op, "id": id})
	}

	serialized := make([]any, len(elements))
	for i := range elements {
		serialized[i] = elements[i]
	}
	scene["elements"] = serialized
	updated, err := json.Marshal(scene)
	if err != nil {
		return nil, nil, err
	}
	return updated, map[string]any{"ok": true, "applied": applied}, nil
}

func forbiddenCanvasChange(key string) bool {
	switch key {
	case "id", "type", "index", "version", "versionNonce", "updated", "isDeleted":
		return true
	default:
		return false
	}
}

func validateNewCanvasElement(element map[string]any) error {
	properties := canvasElementProperties(true)
	for key := range element {
		if _, supported := properties[key]; !supported {
			return fmt.Errorf("unsupported element field %q", key)
		}
	}
	typeName, _ := element["type"].(string)
	typeName = strings.TrimSpace(typeName)
	supported := false
	for _, candidate := range canvasElementTypes {
		if typeName == candidate {
			supported = true
			break
		}
	}
	if !supported {
		return fmt.Errorf("unsupported element type %q", typeName)
	}
	if !finiteJSONNumber(element["x"]) || !finiteJSONNumber(element["y"]) {
		return errors.New("add requires finite numeric x/y coordinates")
	}
	for _, field := range []string{"width", "height", "angle", "strokeWidth", "roughness", "opacity", "fontSize", "lineHeight"} {
		if value, exists := element[field]; exists && !finiteJSONNumber(value) {
			return fmt.Errorf("field %q must be a finite JSON number", field)
		}
	}
	if typeName == "text" {
		if text, ok := element["text"].(string); !ok || text == "" {
			return errors.New("text elements require non-empty text")
		}
	}
	if typeName == "line" || typeName == "arrow" || typeName == "freedraw" {
		if points, exists := element["points"]; exists && !validCanvasPoints(points) {
			return errors.New("points must contain at least two finite numeric [x,y] pairs")
		}
	}
	return nil
}

func validCanvasPoints(value any) bool {
	points, ok := value.([]any)
	if !ok || len(points) < 2 {
		return false
	}
	for _, rawPoint := range points {
		point, ok := rawPoint.([]any)
		if !ok || len(point) != 2 || !finiteJSONNumber(point[0]) || !finiteJSONNumber(point[1]) {
			return false
		}
	}
	return true
}

func validateCanvasChange(key string, value any) error {
	switch key {
	case "x", "y", "width", "height", "angle", "strokeWidth", "roughness", "opacity", "fontSize", "lineHeight":
		if !finiteJSONNumber(value) {
			return fmt.Errorf("field %q must be a finite JSON number", key)
		}
	case "points":
		if !validCanvasPoints(value) {
			return errors.New("points must contain at least two finite numeric [x,y] pairs")
		}
	}
	return nil
}

func canvasElementPositions(elements []map[string]any) map[string]int {
	positions := make(map[string]int, len(elements))
	for index, element := range elements {
		if id, _ := element["id"].(string); id != "" {
			positions[id] = index
		}
	}
	return positions
}

// placeCanvasElement changes array order, which is Excalidraw's canonical
// back-to-front z-order. Empty placement leaves a newly added element in front.
func placeCanvasElement(elements *[]map[string]any, id, position, referenceID string) error {
	position = strings.ToLower(strings.TrimSpace(position))
	if position == "" {
		return nil
	}
	items := *elements
	positions := canvasElementPositions(items)
	current, exists := positions[id]
	if !exists {
		return fmt.Errorf("element %q was not found", id)
	}
	if position != "front" && position != "back" && position != "before" && position != "after" {
		return fmt.Errorf("unsupported position %q", position)
	}
	if (position == "before" || position == "after") && strings.TrimSpace(referenceID) == "" {
		return fmt.Errorf("position %q requires referenceId", position)
	}
	if referenceID == id {
		return errors.New("referenceId must identify a different element")
	}

	moved := items[current]
	items = append(items[:current], items[current+1:]...)
	switch position {
	case "front":
		items = append(items, moved)
	case "back":
		items = append([]map[string]any{moved}, items...)
	case "before", "after":
		positions = canvasElementPositions(items)
		reference, found := positions[strings.TrimSpace(referenceID)]
		if !found {
			return fmt.Errorf("reference element %q was not found", referenceID)
		}
		if position == "after" {
			reference++
		}
		items = append(items, nil)
		copy(items[reference+1:], items[reference:])
		items[reference] = moved
	}
	*elements = items
	return nil
}

func finiteJSONNumber(value any) bool {
	var number float64
	switch typed := value.(type) {
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return false
		}
		number = parsed
	case float64:
		number = typed
	default:
		return false
	}
	return !math.IsNaN(number) && !math.IsInf(number, 0)
}

func setElementDefaults(element map[string]any, offset int) {
	defaults := map[string]any{
		"angle": 0, "strokeColor": "#1e1e1e", "backgroundColor": "transparent",
		"fillStyle": "solid", "strokeWidth": 2, "strokeStyle": "solid", "roughness": 1,
		"opacity": 100, "width": 100, "height": 100, "seed": time.Now().UnixNano() + int64(offset),
		"version": 1, "versionNonce": time.Now().UnixNano() & 0x7fffffff, "isDeleted": false,
		"groupIds": []any{}, "frameId": nil, "roundness": nil, "boundElements": nil,
		"link": nil, "locked": false,
	}
	for key, value := range defaults {
		if _, exists := element[key]; !exists {
			element[key] = value
		}
	}
	typeName, _ := element["type"].(string)
	switch typeName {
	case "line", "arrow":
		if _, exists := element["points"]; !exists {
			element["points"] = []any{[]any{0, 0}, []any{100, 100}}
		}
		for key, value := range map[string]any{"lastCommittedPoint": nil, "startBinding": nil, "endBinding": nil, "startArrowhead": nil} {
			if _, exists := element[key]; !exists {
				element[key] = value
			}
		}
		if _, exists := element["endArrowhead"]; !exists {
			if typeName == "arrow" {
				element["endArrowhead"] = "arrow"
			} else {
				element["endArrowhead"] = nil
			}
		}
		if typeName == "arrow" {
			if _, exists := element["elbowed"]; !exists {
				element["elbowed"] = false
			}
		}
	case "text":
		text, _ := element["text"].(string)
		textDefaults := map[string]any{"originalText": text, "fontSize": 20, "fontFamily": 5, "textAlign": "left", "verticalAlign": "top", "containerId": nil, "autoResize": true, "lineHeight": 1.25}
		for key, value := range textDefaults {
			if _, exists := element[key]; !exists {
				element[key] = value
			}
		}
	case "freedraw":
		if _, exists := element["points"]; !exists {
			element["points"] = []any{[]any{0, 0}, []any{1, 1}}
		}
		if _, exists := element["pressures"]; !exists {
			element["pressures"] = []any{}
		}
		if _, exists := element["simulatePressure"]; !exists {
			element["simulatePressure"] = true
		}
	case "image":
		imageDefaults := map[string]any{"status": "saved", "scale": []any{1, 1}, "crop": nil}
		for key, value := range imageDefaults {
			if _, exists := element[key]; !exists {
				element[key] = value
			}
		}
	case "frame", "magicframe":
		if _, exists := element["name"]; !exists {
			element["name"] = nil
		}
	}
	element["updated"] = time.Now().UnixMilli()
}

func bumpElementVersion(element map[string]any) {
	version := int64(0)
	switch current := element["version"].(type) {
	case json.Number:
		version, _ = current.Int64()
	case float64:
		version = int64(current)
	case int:
		version = int64(current)
	case int64:
		version = current
	}
	element["version"] = version + 1
	element["versionNonce"] = time.Now().UnixNano() & 0x7fffffff
	element["updated"] = time.Now().UnixMilli()
}
