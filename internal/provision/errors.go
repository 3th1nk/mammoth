package provision

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/3th1nk/mammoth/internal/bmc"
	"github.com/3th1nk/mammoth/internal/store"
)

// Execution errors carry a registry code + retryability so the runner can
// decide nack-retry vs terminal failure without knowing layer details.
var (
	ErrCanceled = errors.New("provision: task canceled")

	// ErrInstallNotImplemented: the install pipeline lands in M3; jobs of
	// type install fail fast and explicitly rather than pretending.
	errInstallNotImplemented = errors.New("install pipeline not implemented until M3")
)

// errInfo attaches a classified store.ErrorInfo to any error.
type errInfo struct {
	store.ErrorInfo
}

func (e *errInfo) Error() string { return e.Code + ": " + e.Message }

// Classified extracts the machine-readable triple from an error.
func Classified(err error) store.ErrorInfo {
	var ei *errInfo
	if errors.As(err, &ei) {
		return ei.ErrorInfo
	}
	var bmcErr *bmc.Error
	if errors.As(err, &bmcErr) {
		return store.ErrorInfo{Code: bmcErr.Code(), Message: bmcErr.Error(), Retryable: bmcErr.Retryable()}
	}
	return store.ErrorInfo{Code: "INSTALL_INTERNAL", Message: err.Error(), Retryable: true}
}

// AsClassified reports whether err carries a classified triple (errInfo) and
// returns it. Unlike Classified it does NOT fold bmc.Error or unknown errors
// into a default — the API error mapper needs the distinction (bmc failures
// surface as 502 with their own code; unknown errors must stay 500, only
// classified rejections are 422s).
func AsClassified(err error) (store.ErrorInfo, bool) {
	var ei *errInfo
	if errors.As(err, &ei) {
		return ei.ErrorInfo, true
	}
	return store.ErrorInfo{}, false
}

func classifiedErr(code string, retryable bool, format string, args ...any) error {
	return &errInfo{ErrorInfo: store.ErrorInfo{
		Code: code, Message: fmt.Sprintf(format, args...), Retryable: retryable,
	}}
}

// IsCanceled reports whether err is the cooperative cancel signal.
func IsCanceled(err error) bool { return errors.Is(err, ErrCanceled) }

// action decodes the generic BMC action payload (docs/03-api.md §2).
type action struct {
	Type       string `json:"type"`
	Device     string `json:"device,omitempty"`
	Once       *bool  `json:"once,omitempty"`
	ImageURL   string `json:"image_url,omitempty"`
	EjectAfter bool   `json:"eject_after,omitempty"`
	Probe      string `json:"probe,omitempty"`
	// Boot names the ramdisk probe's carrier (pxe | virtual_media); empty
	// follows the deployment default (docs/06-install-pipeline.md §3.3).
	Boot string `json:"boot,omitempty"`
	// Attributes carries the set_bios_attributes payload (docs/07-bmc.md
	// §6); Confirm is the two-stage confirmation flag — required unless the
	// deployment policy says optional (API-side gate).
	Attributes map[string]any `json:"attributes,omitempty"`
	Confirm    bool           `json:"confirm,omitempty"`
	// Serials / All carry the erase_drives payload (docs/07-bmc.md §6.2):
	// explicit serials, or every physical drive the controller reports.
	// Confirm gates it the same two-stage way.
	Serials []string `json:"serials,omitempty"`
	All     bool     `json:"all,omitempty"`
}

func decodeAction(raw json.RawMessage) (action, error) {
	var a action
	if len(raw) == 0 {
		return a, classifiedErr("SCHEMA_ACTION_REQUIRED", false, "power job requires an action")
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, classifiedErr("SCHEMA_INVALID_ACTION", false, "invalid action payload: %s", err.Error())
	}
	return a, nil
}
