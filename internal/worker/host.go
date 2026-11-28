package worker

import (
	"github.com/semistrict/hotbed/internal/statedir"
)

// hostPrep is what prepareHost readies the machine with.
type hostPrep struct {
	cfg, effCfg statedir.WorkerConfig
	ecfg        *EngineConfig
	workSlice   string
}
