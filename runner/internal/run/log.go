package run

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// campaignLogName is the per-bundle log every session appends to.
const campaignLogName = "campaign.log"

// Notef prints a bash-style note — `== [HH:MM:SS] msg`, the clock in UTC —
// the line format every operator reading a campaign log already knows from
// the note() of the bash campaign runner this package replaces.
func Notef(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "== [%s] %s\n", time.Now().UTC().Format("15:04:05"), fmt.Sprintf(format, args...))
}

// OpenCampaignLog opens <resultsDir>/campaign.log for appending. Append, not
// truncate: a campaign that is resumed twice leaves all three sessions in one
// file, in the order they happened.
func OpenCampaignLog(resultsDir string) (*os.File, error) {
	return os.OpenFile(filepath.Join(resultsDir, campaignLogName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}
