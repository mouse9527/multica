// Added by the mouse9527/multica fork: the project Evolution files its issues
// into (M-231).

package evolution

import "testing"

func TestConfigOutputProjectID(t *testing.T) {
	const id = "0b0e6a5e-4c1f-4b7e-9a51-2d3c4e5f6a7b"

	if got := DefaultConfig().OutputProjectID; got != "" {
		t.Fatalf("default output_project_id %q, want none", got)
	}
	// Stored values are normalized; one that is not a uuid reads as none.
	stored := ConfigFromSettings([]byte(`{"evolution": {"output_project_id": " 0B0E6A5E-4C1F-4B7E-9A51-2D3C4E5F6A7B "}}`))
	if stored.OutputProjectID != id {
		t.Errorf("stored output_project_id read as %q, want %q", stored.OutputProjectID, id)
	}
	if got := ConfigFromSettings([]byte(`{"evolution": {"output_project_id": "nope"}}`)).OutputProjectID; got != "" {
		t.Errorf("a malformed stored id read as %q, want none", got)
	}

	cfg := DefaultConfig()
	cfg.OutputProjectID = "nope"
	if err := cfg.Validate(); !IsInvalidConfig(err) {
		t.Errorf("a malformed id must be rejected, got %v", err)
	}
	cfg.OutputProjectID = id
	if err := cfg.Validate(); err != nil {
		t.Errorf("a uuid must pass: %v", err)
	}
	cfg.OutputProjectID = ""
	if err := cfg.Validate(); err != nil {
		t.Errorf("none must pass: %v", err)
	}
	cfg.OutputProjectID = " " + "0B0E6A5E-4C1F-4B7E-9A51-2D3C4E5F6A7B"
	if got := cfg.normalized().OutputProjectID; got != id {
		t.Errorf("normalized output_project_id %q, want %q", got, id)
	}
}
