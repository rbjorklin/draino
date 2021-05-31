package kubernetes

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"go.opencensus.io/stats"
	"go.opencensus.io/stats/view"
	"go.opencensus.io/tag"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/util/wait"
)

type GlobalBlocker interface {
	IsBlocked() (bool, string)
	AddBlocker(name string, checkFunc ComputeBlockStateFunction, period time.Duration) error
	GetBlockStateCacheAccessor() map[string]GetBlockStateFunction
	Run(stopCh <-chan struct{})
}

// ComputeBlockStateFunction a function that would analyse the system state and return true if we should lock draino to prevent any cordon/drain activity
type ComputeBlockStateFunction func() bool

// GetBlockStateFunction a function that would return the current state of the lock using the cached value (no analysis)
type GetBlockStateFunction func() bool

type blocker struct {
	name       string
	checkFunc  ComputeBlockStateFunction
	period     time.Duration
	blockState bool
}

func (l *blocker) updateBlockState() {
	l.blockState = l.checkFunc()
}

var _ GlobalBlocker = &GlobalBlocksRunner{}

type GlobalBlocksRunner struct {
	sync.Mutex
	blockers []*blocker
	started  bool
	logger   *zap.Logger
}

func NewGlobalBlocker() *GlobalBlocksRunner {
	return &GlobalBlocksRunner{}
}

var (
	MeasureBlocker = stats.Int64("draino/global_block", "GlobalBlock indicator.", stats.UnitDimensionless)
)

func (g *GlobalBlocksRunner) Run(stopCh <-chan struct{}) {
	if g.started {
		g.logger.Error("GlobalBlocker run twice")
		return
	}
	tagBlock, _ := tag.NewKey("block")
	blockerView := &view.View{
		Name:        "global_block",
		Measure:     MeasureBlocker,
		Description: "State of global blocks",
		Aggregation: view.LastValue(),
		TagKeys:     []tag.Key{tagBlock},
	}
	view.Register(blockerView)
	g.Lock()
	defer g.Unlock()
	g.started = true
	for i := range g.blockers {
		localBlocker := g.blockers[i]
		go wait.Until(
			func() {
				// Perform Check
				localBlocker.updateBlockState()
				val := int64(0)
				if localBlocker.blockState {
					val = 1
				}
				// Observability
				tag, _ := tag.New(context.Background(), tag.Upsert(tagBlock, localBlocker.name))
				stats.Record(tag, MeasureBlocker.M(val))
			},
			localBlocker.period,
			stopCh)
	}
}

func (g *GlobalBlocksRunner) AddBlocker(name string, checkFunc ComputeBlockStateFunction, period time.Duration) error {
	g.Lock()
	defer g.Unlock()
	if g.started {
		errors.New("Can't add a Blocker once the GlobalBlocker has been started")
	}
	g.blockers = append(g.blockers, &blocker{
		name:       name,
		checkFunc:  checkFunc,
		period:     period,
		blockState: false,
	})
	return nil
}

func (g *GlobalBlocksRunner) GetBlockStateCacheAccessor() map[string]GetBlockStateFunction {
	m := map[string]GetBlockStateFunction{}
	for i := range g.blockers {
		l := g.blockers[i]
		m[l.name] = func() bool { return l.blockState }
	}
	return m
}

func (g *GlobalBlocksRunner) IsBlocked() (bool, string) {
	for _, l := range g.blockers {
		if l.blockState {
			return true, l.name
		}
	}
	return false, ""
}

func MaxNotReadyNodesCheckFunc(max int, percent bool, store RuntimeObjectStore) ComputeBlockStateFunction {
	return func() bool {
		if !store.HasSynced() {
			return false
		}
		if store.Nodes() == nil {
			return false
		}
		notReadyCount := 0
		nodeList := store.Nodes().ListNodes()
		for _, n := range nodeList {
			if ready, _ := GetReadinessState(n); !ready {
				notReadyCount++
			}
		}
		blocked := false
		if percent {
			blocked = math.Ceil(100*float64(notReadyCount)/float64(len(nodeList))) <= float64(max)
		} else {
			blocked = notReadyCount < max
		}
		return blocked
	}
}
