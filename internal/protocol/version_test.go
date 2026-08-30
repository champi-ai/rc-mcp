package protocol

import "testing"

func TestVersionConstant(t *testing.T) {
	if Version != "2" {
		t.Fatalf("Version = %q, want %q", Version, "2")
	}
}

func TestIsSupportedVersion(t *testing.T) {
	if !IsSupportedVersion("2") {
		t.Error("expected the current version \"2\" to be supported")
	}
	if !IsSupportedVersion("1") {
		t.Error("expected the prior version \"1\" to remain supported for backward compatibility")
	}
	if IsSupportedVersion("3") {
		t.Error("expected version \"3\" to be unsupported")
	}
	if IsSupportedVersion("") {
		t.Error("expected empty version to be unsupported")
	}
}

// TestIsSupportedVersion_DroppedLegacyVersion exercises the documented
// upgrade-path endgame (Version's doc comment, step 3): once an operator
// drops an old version from SupportedVersions, a peer still declaring it
// is cleanly rejected rather than silently mishandled.
func TestIsSupportedVersion_DroppedLegacyVersion(t *testing.T) {
	original := SupportedVersions
	SupportedVersions = []string{"2"}
	defer func() { SupportedVersions = original }()

	if IsSupportedVersion("1") {
		t.Error("expected \"1\" to be rejected once dropped from SupportedVersions")
	}
	if !IsSupportedVersion("2") {
		t.Error("expected \"2\" to remain supported")
	}

	env := VersionMismatchEnvelope("1")
	payload, ok := env.Payload.(ErrorPayload)
	if !ok || payload.Code != "version_mismatch" {
		t.Fatalf("VersionMismatchEnvelope(\"1\") = %+v, want a version_mismatch error", env)
	}
}

func TestVersionMismatchEnvelope(t *testing.T) {
	env := VersionMismatchEnvelope("2")

	if env.Type != MsgError {
		t.Fatalf("type = %q, want %q", env.Type, MsgError)
	}

	payload, ok := env.Payload.(ErrorPayload)
	if !ok {
		t.Fatalf("payload type = %T, want ErrorPayload", env.Payload)
	}
	if payload.Code != "version_mismatch" {
		t.Errorf("code = %q, want %q", payload.Code, "version_mismatch")
	}
	if payload.Message == "" {
		t.Error("expected a non-empty message")
	}
}
