package listenaddr

import (
	"context"
	"encoding/json"
	"os/exec"
	"time"
)

// ReadTailscaleStatus runs `tailscale status --json`. Tests may replace this.
var ReadTailscaleStatus = execTailscaleStatus

func execTailscaleStatus(ctx context.Context) ([]byte, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "tailscale", "status", "--json")
	return cmd.Output()
}

type tailscaleStatusJSON struct {
	Self struct {
		Online bool `json:"Online"`
	} `json:"Self"`
}

// SelfOnline reports Tailscale's Self.Online field.
// checked is false when the CLI is missing, the command fails, or JSON is unusable.
func SelfOnline(ctx context.Context) (online bool, checked bool) {
	raw, err := ReadTailscaleStatus(ctx)
	if err != nil || len(raw) == 0 {
		return false, false
	}
	var st tailscaleStatusJSON
	if err := json.Unmarshal(raw, &st); err != nil {
		return false, false
	}
	return st.Self.Online, true
}

// OfflineMessage is the loud warning when 100.x is assigned but Tailscale is down.
const OfflineMessage = "tailnet is offline (Self.Online=false); the 100.x address is assigned but peers cannot reach you"
