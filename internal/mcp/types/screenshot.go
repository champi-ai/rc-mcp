package types

// ScreenshotCaptureInput is the input schema for screenshot_capture
// (docs/specs/backend.md Section 3.2.1).
type ScreenshotCaptureInput struct {
	ClientID string  `json:"clientId"`
	Display  *string `json:"display,omitempty"`
	Monitor  *int    `json:"monitor,omitempty"`
	Quality  *int    `json:"quality,omitempty"`
	MaxWidth *int    `json:"maxWidth,omitempty"`
}

// ScreenshotCaptureOutput describes the captured image's metadata; the PNG
// bytes themselves are sent as a binary WS frame (FrameScreenshotPNG), not
// inlined here -- the server base64-encodes that frame into the MCP image
// content response.
type ScreenshotCaptureOutput struct {
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	MimeType string `json:"mimeType"`
	ClientID string `json:"clientId"`
}

// ScreenshotWatchInput is the input schema for screenshot_watch
// (Section 3.2.2).
type ScreenshotWatchInput struct {
	ClientID     string  `json:"clientId"`
	Display      *string `json:"display,omitempty"`
	Monitor      *int    `json:"monitor,omitempty"`
	IntervalMs   *int    `json:"intervalMs,omitempty"`
	MaxFrames    *int    `json:"maxFrames,omitempty"`
	DurationSecs *int    `json:"durationSecs,omitempty"`
	MaxWidth     *int    `json:"maxWidth,omitempty"`
	Quality      *int    `json:"quality,omitempty"`
}

// ScreenshotWatchAck is the immediate tools/call result for screenshot_watch
// (dispatch pattern (a) -- Section 9), returned as soon as the agent
// acknowledges the dispatch.
type ScreenshotWatchAck struct {
	JobID    string `json:"jobId"`
	ClientID string `json:"clientId"`
}

// ScreenshotWatchOutcome is the terminal job outcome for screenshot_watch,
// delivered as the terminal notifications/progress event and persisted at
// job://{id}, per Section 3.2.2.
type ScreenshotWatchOutcome struct {
	JobID          string `json:"jobId"`
	FramesCaptured int    `json:"framesCaptured"`
	DurationMs     int64  `json:"durationMs"`
	StoppedReason  string `json:"stoppedReason"` // maxFrames | duration | cancelled | agent_disconnect
	ClientID       string `json:"clientId"`
}
