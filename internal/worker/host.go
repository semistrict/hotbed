package worker

import (
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/storage"
)

// hostPrep is what prepareHost readies the machine with.
type hostPrep struct {
	cfg, effCfg statedir.WorkerConfig
	ecfg        *EngineConfig
	store       storage.Store
	workSlice   string
}
