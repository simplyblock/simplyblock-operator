package controlplane

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/simplyblock/atlas/errs"
)

// respError turns a non-success control-plane response into an error, mapping
// 404 to errs.ErrNotFound so callers can match with errors.Is. what names the
// resource or operation, e.g. `storage node "abc"`.
func respError(what string, code int, body []byte) error {
	if code == http.StatusNotFound {
		return fmt.Errorf("%s: %w", what, errs.ErrNotFound)
	}
	return fmt.Errorf("%s: control-plane returned %d: %s", what, code, strings.TrimSpace(string(body)))
}
