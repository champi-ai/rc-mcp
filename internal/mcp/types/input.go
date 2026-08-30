package types

// InputKeyInput is the input schema for input_key (Section 19: keyboard/
// mouse input injection).
type InputKeyInput struct {
	ClientID string `json:"clientId"`
	// Key is an xdotool-syntax key spec, e.g. "Return", "ctrl+c", "F5".
	Key string `json:"key"`
}

// InputKeyOutput is the output schema for input_key.
type InputKeyOutput struct {
	ClientID string `json:"clientId"`
}

// InputMouseClickInput is the input schema for input_mouse_click.
type InputMouseClickInput struct {
	ClientID string `json:"clientId"`
	X        int    `json:"x"`
	Y        int    `json:"y"`
	// Button is "left" | "middle" | "right"; default "left".
	Button *string `json:"button,omitempty"`
}

// InputMouseClickOutput is the output schema for input_mouse_click.
type InputMouseClickOutput struct {
	ClientID string `json:"clientId"`
}

// InputMouseMoveInput is the input schema for input_mouse_move.
type InputMouseMoveInput struct {
	ClientID string `json:"clientId"`
	X        int    `json:"x"`
	Y        int    `json:"y"`
}

// InputMouseMoveOutput is the output schema for input_mouse_move.
type InputMouseMoveOutput struct {
	ClientID string `json:"clientId"`
}

// InputTypeInput is the input schema for input_type.
type InputTypeInput struct {
	ClientID string `json:"clientId"`
	Text     string `json:"text"`
}

// InputTypeOutput is the output schema for input_type.
type InputTypeOutput struct {
	ClientID string `json:"clientId"`
}
