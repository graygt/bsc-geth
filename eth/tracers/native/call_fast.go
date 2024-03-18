package native

import (
	"encoding/json"
	"math/big"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/tracers"
)

func init() {
	tracers.DefaultDirectory.Register("fastCallTracer", newFastCallTracer, false)
}

type simplifiedCallFrame struct {
	Output []byte `json:"output,omitempty"` // Only store the output
}

type fastCallTracer struct {
	noopTracer
	output    []byte
	gasLimit  uint64
	interrupt atomic.Bool // Atomic flag to signal execution interruption
	reason    error       // Textual reason for the interruption
}

func newFastCallTracer(ctx *tracers.Context, cfg json.RawMessage) (tracers.Tracer, error) {
	// Simplified tracer does not use configuration
	return &fastCallTracer{}, nil
}

func (t *fastCallTracer) CaptureStart(env *vm.EVM, from common.Address, to common.Address, create bool, input []byte, gas uint64, value *big.Int) {
	// Initialize or reset the output at the start of a transaction
	t.output = nil
	t.gasLimit = gas
}

func (t *fastCallTracer) CaptureEnd(output []byte, gasUsed uint64, err error) {
	// Directly store the output at the end of the top-level call
	t.output = common.CopyBytes(output)
}

// Override the CaptureState method to do nothing for efficiency
func (t *fastCallTracer) CaptureState(pc uint64, op vm.OpCode, gas, cost uint64, scope *vm.ScopeContext, rData []byte, depth int, err error) {
	// No operation, since internal state changes are not of interest
}

// CaptureEnter and CaptureExit are overridden to do nothing since internal calls are not tracked
func (t *fastCallTracer) CaptureEnter(typ vm.OpCode, from common.Address, to common.Address, input []byte, gas uint64, value *big.Int) {
	// No operation, since internal calls are not tracked
}

func (t *fastCallTracer) CaptureExit(output []byte, gasUsed uint64, err error) {
	// No operation, since internal calls are not tracked
}

func (t *fastCallTracer) GetResult() (json.RawMessage, error) {
	// Return the output directly without any additional processing
	res, err := json.Marshal(simplifiedCallFrame{Output: t.output})
	if err != nil {
		return nil, err
	}
	return json.RawMessage(res), t.reason
}

func (t *fastCallTracer) Stop(err error) {
	t.reason = err
	t.interrupt.Store(true)
}
